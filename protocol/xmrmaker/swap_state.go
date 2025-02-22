// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

// Package xmrmaker manages the swap state of individual swaps where the local swapd
// instance is offering Monero and accepting Ethereum assets in return.
package xmrmaker

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/cockroachdb/apd/v3"
	"github.com/ethereum/go-ethereum"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/fatih/color"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/athanorlabs/atomic-swap/coins"
	"github.com/athanorlabs/atomic-swap/common"
	"github.com/athanorlabs/atomic-swap/common/types"
	mcrypto "github.com/athanorlabs/atomic-swap/crypto/monero"
	"github.com/athanorlabs/atomic-swap/crypto/secp256k1"
	"github.com/athanorlabs/atomic-swap/db"
	"github.com/athanorlabs/atomic-swap/dleq"
	contracts "github.com/athanorlabs/atomic-swap/ethereum"
	"github.com/athanorlabs/atomic-swap/ethereum/watcher"
	"github.com/athanorlabs/atomic-swap/monero"
	"github.com/athanorlabs/atomic-swap/net/message"
	pcommon "github.com/athanorlabs/atomic-swap/protocol"
	"github.com/athanorlabs/atomic-swap/protocol/backend"
	"github.com/athanorlabs/atomic-swap/protocol/swap"
	pswap "github.com/athanorlabs/atomic-swap/protocol/swap"
	"github.com/athanorlabs/atomic-swap/protocol/txsender"
	"github.com/athanorlabs/atomic-swap/protocol/xmrmaker/offers"
)

var (
	readyTopic    = common.GetTopic(common.ReadyEventSignature)
	claimedTopic  = common.GetTopic(common.ClaimedEventSignature)
	refundedTopic = common.GetTopic(common.RefundedEventSignature)
)

type swapState struct {
	backend.Backend
	sender txsender.Sender

	ctx    context.Context
	cancel context.CancelFunc

	info         *pswap.Info
	offer        *types.Offer
	offerExtra   *types.OfferExtra
	offerManager *offers.Manager

	// our keys for this session
	dleqProof    *dleq.Proof
	secp256k1Pub *secp256k1.PublicKey
	privkeys     *mcrypto.PrivateKeyPair
	pubkeys      *mcrypto.PublicKeyPair

	// swap contract and timeouts in it
	swapCreator     *contracts.SwapCreator
	swapCreatorAddr ethcommon.Address
	contractSwapID  [32]byte
	contractSwap    *contracts.SwapCreatorSwap
	t0, t1          time.Time

	// XMRTaker's keys for this session
	xmrtakerPublicSpendKey     *mcrypto.PublicKey
	xmrtakerPrivateViewKey     *mcrypto.PrivateViewKey
	xmrtakerSecp256K1PublicKey *secp256k1.PublicKey
	moneroStartHeight          uint64 // height of the monero blockchain when the swap is started

	// tracks the state of the swap
	nextExpectedEvent EventType

	readyWatcher *watcher.EventFilter

	// channels

	// channel for swap events
	// the event handler in event.go ensures only one event is being handled at a time
	eventCh chan Event
	// channel for `Ready` logs seen on-chain
	logReadyCh chan ethtypes.Log
	// channel for `Refunded` logs seen on-chain
	logRefundedCh chan ethtypes.Log
	// signals the t0 expiration handler to return
	readyCh chan struct{}
	// signals to the creator xmrmaker instance that it can delete this swap
	done chan struct{}
}

// newSwapStateFromStart returns a new *swapState for a fresh swap.
func newSwapStateFromStart(
	b backend.Backend,
	takerPeerID peer.ID,
	offer *types.Offer,
	offerExtra *types.OfferExtra,
	om *offers.Manager,
	providesAmount *coins.PiconeroAmount,
	desiredAmount coins.EthAssetAmount,
) (*swapState, error) {
	// at this point, we've received the counterparty's keys,
	// and will send our own after this function returns.
	// see HandleInitiateMessage().
	stage := types.KeysExchanged
	if offerExtra.StatusCh == nil {
		offerExtra.StatusCh = make(chan types.Status, 7)
	}

	if offerExtra.UseRelayer {
		if err := b.RecoveryDB().PutSwapRelayerInfo(offer.ID, offerExtra); err != nil {
			return nil, err
		}
	}

	moneroStartHeight, err := b.XMRClient().GetHeight()
	if err != nil {
		return nil, err
	}
	// reduce the scan height a little in case there is a block reorg
	if moneroStartHeight >= monero.MinSpendConfirmations {
		moneroStartHeight -= monero.MinSpendConfirmations
	}

	ethHeader, err := b.ETHClient().Raw().HeaderByNumber(b.Ctx(), nil)
	if err != nil {
		return nil, err
	}

	info := pswap.NewInfo(
		takerPeerID,
		offer.ID,
		coins.ProvidesXMR,
		providesAmount.AsMonero(),
		desiredAmount.AsStandard(),
		offer.ExchangeRate,
		offer.EthAsset,
		stage,
		moneroStartHeight,
		offerExtra.StatusCh,
	)

	if err = b.SwapManager().AddSwap(info); err != nil {
		return nil, err
	}

	s, err := newSwapState(
		b,
		offer,
		offerExtra,
		om,
		ethHeader.Number,
		moneroStartHeight,
		info,
	)
	if err != nil {
		return nil, err
	}

	err = s.generateAndSetKeys()
	if err != nil {
		return nil, err
	}

	offerExtra.StatusCh <- stage
	return s, nil
}

// checkIfAlreadyClaimed returns true if the ETH has already been
// claimed by us, false otherwise.
func checkIfAlreadyClaimed(
	b backend.Backend,
	ethSwapInfo *db.EthereumSwapInfo,
) (bool, error) {
	// check if swap actually completed and we didn't realize for some reason
	// this could happen if we restart from an ongoing swap
	contract, err := contracts.NewSwapCreator(ethSwapInfo.SwapCreatorAddr, b.ETHClient().Raw())
	if err != nil {
		return false, err
	}

	stage, err := contract.Swaps(b.ETHClient().CallOpts(b.Ctx()), ethSwapInfo.SwapID)
	if err != nil {
		return false, err
	}

	switch stage {
	case contracts.StageInvalid:
		// this should never happen
		return false, fmt.Errorf("%w: contract swap ID: %s", errSwapDoesNotExist, ethSwapInfo.SwapID)
	case contracts.StageCompleted:
		// check if we already claimed, or if the swap was refunded
	case contracts.StagePending, contracts.StageReady:
		return false, nil
	default:
		panic("Unhandled stage value")
	}

	filterQuery := ethereum.FilterQuery{
		FromBlock: ethSwapInfo.StartNumber,
		Addresses: []ethcommon.Address{ethSwapInfo.SwapCreatorAddr},
	}

	claimedTopic := common.GetTopic(common.ClaimedEventSignature)

	// let's see if we have logs
	logs, err := b.ETHClient().Raw().FilterLogs(b.Ctx(), filterQuery)
	if err != nil {
		return false, fmt.Errorf("failed to filter logs for topic %s: %s", claimedTopic, err)
	}

	log.Debugf("filtered for logs from block %s to head", filterQuery.FromBlock)

	var foundClaimed bool
	for _, l := range logs {
		l := l
		if l.Topics[0] != claimedTopic {
			continue
		}

		if l.Removed {
			log.Debugf("found removed log: tx hash %s", l.TxHash)
			continue
		}

		err = pcommon.CheckSwapID(&l, claimedTopic, ethSwapInfo.SwapID)
		if errors.Is(err, pcommon.ErrLogNotForUs) {
			continue
		}

		log.Infof("found Claimed log in block %d", l.BlockNumber)
		foundClaimed = true
		break
	}

	return foundClaimed, nil
}

// completeSwap marks the swap as completed and deletes it from the db.
func completeSwap(info *swap.Info, b backend.Backend, om *offers.Manager) error {
	// set swap to completed
	info.SetStatus(types.CompletedSuccess)
	err := b.SwapManager().CompleteOngoingSwap(info)
	if err != nil {
		return fmt.Errorf("failed to mark swap %s as completed: %s", info.OfferID, err)
	}

	err = om.DeleteOffer(info.OfferID)
	if err != nil {
		return fmt.Errorf("failed to delete offer %s from db: %s", info.OfferID, err)
	}

	err = b.RecoveryDB().DeleteSwap(info.OfferID)
	if err != nil {
		return fmt.Errorf("failed to delete temporary swap info %s from db: %s", info.OfferID, err)
	}

	exitLog := color.New(color.Bold).Sprintf("**swap completed successfully: id=%s**", info.OfferID)
	log.Info(exitLog)
	return nil
}

// newSwapStateFromOngoing returns a new *swapState given information about a swap
// that's ongoing, but not yet completed.
func newSwapStateFromOngoing(
	b backend.Backend,
	offer *types.Offer,
	offerExtra *types.OfferExtra,
	om *offers.Manager,
	ethSwapInfo *db.EthereumSwapInfo,
	info *pswap.Info,
	sk *mcrypto.PrivateKeyPair,
) (*swapState, error) {
	alreadyClaimed, err := checkIfAlreadyClaimed(b, ethSwapInfo)
	if err != nil {
		return nil, err
	}

	if alreadyClaimed {
		err = completeSwap(info, b, om)
		if err != nil {
			return nil, fmt.Errorf("failed to complete swap: %w", err)
		}

		// although this doesn't look like an error, we need to return an error
		// so the caller knows the swap is completed
		return nil, errors.New("swap was already completed successfully")
	}

	// TODO: do we want to support the case where the ETH has been locked,
	// but we haven't locked yet?
	if info.Status != types.XMRLocked {
		return nil, errInvalidStageForRecovery
	}

	log.Debugf("restarting swap from eth block number %s", ethSwapInfo.StartNumber)
	s, err := newSwapState(
		b, offer, offerExtra, om, ethSwapInfo.StartNumber, info.MoneroStartHeight, info,
	)
	if err != nil {
		return nil, err
	}

	err = s.setContract(ethSwapInfo.SwapCreatorAddr)
	if err != nil {
		return nil, err
	}

	s.setTimeouts(ethSwapInfo.Swap.Timeout0, ethSwapInfo.Swap.Timeout1)
	s.privkeys = sk
	s.pubkeys = sk.PublicKeyPair()
	s.contractSwapID = ethSwapInfo.SwapID
	s.contractSwap = ethSwapInfo.Swap
	return s, nil
}

func newSwapState(
	b backend.Backend,
	offer *types.Offer,
	offerExtra *types.OfferExtra,
	om *offers.Manager,
	ethStartNumber *big.Int,
	moneroStartNumber uint64,
	info *pswap.Info,
) (*swapState, error) {
	var sender txsender.Sender
	if offer.EthAsset.IsToken() {
		erc20Contract, err := contracts.NewIERC20(offer.EthAsset.Address(), b.ETHClient().Raw())
		if err != nil {
			return nil, err
		}

		sender, err = b.NewTxSender(offer.EthAsset.Address(), erc20Contract)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		sender, err = b.NewTxSender(offer.EthAsset.Address(), nil)
		if err != nil {
			return nil, err
		}
	}

	// set up ethereum event watchers
	const logChSize = 16 // arbitrary, we just don't want the watcher to block on writing
	logReadyCh := make(chan ethtypes.Log, logChSize)
	logRefundedCh := make(chan ethtypes.Log, logChSize)

	// Create per swap context that is canceled when the swap completes
	ctx, cancel := context.WithCancel(b.Ctx())

	readyWatcher := watcher.NewEventFilter(
		ctx,
		b.ETHClient().Raw(),
		b.SwapCreatorAddr(),
		ethStartNumber,
		readyTopic,
		logReadyCh,
	)

	refundedWatcher := watcher.NewEventFilter(
		ctx,
		b.ETHClient().Raw(),
		b.SwapCreatorAddr(),
		ethStartNumber,
		refundedTopic,
		logRefundedCh,
	)

	err := readyWatcher.Start()
	if err != nil {
		cancel()
		return nil, err
	}

	err = refundedWatcher.Start()
	if err != nil {
		cancel()
		return nil, err
	}

	// note: if this is recovering an ongoing swap, this will only
	// be invoked if our status is XMRLocked; ie. we've locked XMR,
	// but not yet claimed or refunded.
	//
	// dleqProof and secp256k1Pub are never set, as they are only used
	// in the swap steps before XMR is locked.
	//
	// similarly, xmrtakerPublicKeys and xmrtakerSecp256K1PublicKey are
	// also never set, as they're only used to check the contract
	// before we lock XMR.
	s := &swapState{
		ctx:               ctx,
		cancel:            cancel,
		Backend:           b,
		sender:            sender,
		offer:             offer,
		offerExtra:        offerExtra,
		offerManager:      om,
		moneroStartHeight: moneroStartNumber,
		nextExpectedEvent: nextExpectedEventFromStatus(info.Status),
		logReadyCh:        logReadyCh,
		logRefundedCh:     logRefundedCh,
		eventCh:           make(chan Event, 1),
		readyCh:           make(chan struct{}),
		info:              info,
		done:              make(chan struct{}),
		readyWatcher:      readyWatcher,
	}

	go s.runHandleEvents()
	go s.runContractEventWatcher()
	return s, nil
}

// SendKeysMessage ...
func (s *swapState) SendKeysMessage() common.Message {
	return &message.SendKeysMessage{
		ProvidedAmount:     s.info.ProvidedAmount,
		PublicSpendKey:     s.pubkeys.SpendKey(),
		PrivateViewKey:     s.privkeys.ViewKey(),
		DLEqProof:          s.dleqProof.Proof(),
		Secp256k1PublicKey: s.secp256k1Pub,
		EthAddress:         s.ETHClient().Address(),
	}
}

// ExpectedAmount returns the amount received, or expected to be received, at the end of the swap
func (s *swapState) ExpectedAmount() *apd.Decimal {
	return s.info.ExpectedAmount
}

// OfferID returns the ID of the swap
func (s *swapState) OfferID() types.Hash {
	return s.info.OfferID
}

// Exit is called by the network when the protocol stream closes, or if the swap_refund RPC endpoint is called.
// It exists the swap by refunding if necessary. If no locking has been done, it simply aborts the swap.
// If the swap already completed successfully, this function does not do anything regarding the protocol.
func (s *swapState) Exit() error {
	event := newEventExit()
	s.eventCh <- event
	return <-event.errCh
}

// exit is the same as Exit, but assumes the calling code block already holds the swapState lock.
func (s *swapState) exit() error {
	log.Debugf("attempting to exit swap: nextExpectedEvent=%v", s.nextExpectedEvent)

	defer func() {
		s.CloseProtocolStream(s.OfferID())

		err := s.SwapManager().CompleteOngoingSwap(s.info)
		if err != nil {
			log.Warnf("failed to mark swap %s as completed: %s", s.offer.ID, err)
			return
		}

		log.Infof("exit status %s", s.info.Status)

		if s.info.Status != types.CompletedSuccess && s.offer.IsSet() {
			// re-add offer, as it wasn't taken successfully
			_, err = s.offerManager.AddOffer(s.offer, s.offerExtra.UseRelayer)
			if err != nil {
				log.Warnf("failed to re-add offer %s: %s", s.offer.ID, err)
			}

			log.Debugf("re-added offer %s", s.offer.ID)
		} else if s.info.Status == types.CompletedSuccess {
			err = s.offerManager.DeleteOffer(s.offer.ID)
			if err != nil {
				log.Warnf("failed to delete offer %s from db: %s", s.offer.ID, err)
			}
		}

		err = s.Backend.RecoveryDB().DeleteSwap(s.offer.ID)
		if err != nil {
			log.Warnf("failed to delete temporary swap info %s from db: %s", s.offer.ID, err)
		}

		// Stop all per-swap goroutines
		s.cancel()
		close(s.done)

		var exitLog string
		switch s.info.Status {
		case types.CompletedSuccess:
			exitLog = color.New(color.Bold).Sprintf("**swap completed successfully: id=%s**", s.OfferID())
		case types.CompletedRefund:
			exitLog = color.New(color.Bold).Sprintf("**swap refunded successfully: id=%s**", s.OfferID())
		case types.CompletedAbort:
			exitLog = color.New(color.Bold).Sprintf("**swap aborted: id=%s**", s.OfferID())
		}

		log.Info(exitLog)
	}()

	switch s.nextExpectedEvent {
	case EventETHLockedType:
		// we were waiting for the contract to be deployed, but haven't
		// locked out funds yet, so we're fine.
		s.clearNextExpectedEvent(types.CompletedAbort)
		return nil
	case EventContractReadyType:
		// this case takes control of the event channel.
		// the next event will either be EventContractReady or EventETHRefunded.

		log.Infof("waiting for EventETHRefunded or EventContractReady")

		var err error
		event := <-s.eventCh

		switch e := event.(type) {
		case *EventETHRefunded:
			defer close(e.errCh)
			log.Infof("got EventETHRefunded")
			err = s.handleEventETHRefunded(e)
		case *EventContractReady:
			defer close(e.errCh)
			log.Infof("got EventContractReady")
			err = s.handleEventContractReady()
		}
		if err != nil {
			return err
		}

		return nil
	case EventNoneType:
		// we already completed the swap, do nothing
		return nil
	default:
		s.clearNextExpectedEvent(types.CompletedAbort)
		log.Errorf("unexpected nextExpectedEvent in Exit: type=%s", s.nextExpectedEvent)
		return errUnexpectedMessageType
	}
}

func (s *swapState) reclaimMonero(skA *mcrypto.PrivateSpendKey) error {
	// write counterparty swap privkey to disk in case something goes wrong
	err := s.Backend.RecoveryDB().PutCounterpartySwapPrivateKey(s.OfferID(), skA)
	if err != nil {
		return err
	}

	if s.xmrtakerPublicSpendKey == nil || s.xmrtakerPrivateViewKey == nil {
		s.xmrtakerPublicSpendKey, s.xmrtakerPrivateViewKey, err = s.RecoveryDB().GetCounterpartySwapKeys(s.OfferID())
		if err != nil {
			return fmt.Errorf("failed to get counterparty public keypair: %w", err)
		}
	}

	kpAB := pcommon.GetClaimKeypair(
		skA, s.privkeys.SpendKey(),
		s.xmrtakerPrivateViewKey, s.privkeys.ViewKey(),
	)

	return pcommon.ClaimMonero(
		s.ctx,
		s.Env(),
		s.info,
		s.XMRClient(),
		kpAB,
		s.XMRClient().PrimaryAddress(),
		false, // always sweep back to our primary address
		s.Backend.SwapManager(),
	)
}

// generateKeys generates XMRMaker's spend and view keys (s_b, v_b)
// It returns XMRMaker's public spend key and his private view key, so that XMRTaker can see
// if the funds are locked.
func (s *swapState) generateAndSetKeys() error {
	if s.privkeys != nil {
		panic("generateAndSetKeys should only be called once")
	}

	keysAndProof, err := generateKeys()
	if err != nil {
		return err
	}

	s.dleqProof = keysAndProof.DLEqProof
	s.secp256k1Pub = keysAndProof.Secp256k1PublicKey
	s.privkeys = keysAndProof.PrivateKeyPair
	s.pubkeys = keysAndProof.PublicKeyPair

	return s.Backend.RecoveryDB().PutSwapPrivateKey(s.OfferID(), s.privkeys.SpendKey())
}

func generateKeys() (*pcommon.KeysAndProof, error) {
	return pcommon.GenerateKeysAndProof()
}

// getSecret secrets returns the current secret scalar used to unlock funds from the contract.
func (s *swapState) getSecret() [32]byte {
	if s.dleqProof != nil {
		return s.dleqProof.Secret()
	}

	var secret [32]byte
	copy(secret[:], common.Reverse(s.privkeys.SpendKeyBytes()))
	return secret
}

// setXMRTakerKeys sets XMRTaker's public spend and private view key
func (s *swapState) setXMRTakerKeys(
	sk *mcrypto.PublicKey,
	vk *mcrypto.PrivateViewKey,
	secp256k1Pub *secp256k1.PublicKey,
) error {
	s.xmrtakerPublicSpendKey = sk
	s.xmrtakerPrivateViewKey = vk
	s.xmrtakerSecp256K1PublicKey = secp256k1Pub
	return s.RecoveryDB().PutCounterpartySwapKeys(s.OfferID(), sk, vk)
}

// setContract sets the swapCreator in which XMRTaker has locked her ETH.
func (s *swapState) setContract(address ethcommon.Address) error {
	s.swapCreatorAddr = address

	var err error
	s.swapCreator, err = s.NewSwapCreator(address)
	if err != nil {
		return err
	}

	s.sender.SetSwapCreatorAddr(address)
	s.sender.SetSwapCreator(s.swapCreator)
	return nil
}

// lockFunds locks XMRMaker's funds in the monero account specified by public key
// (S_a + S_b), viewable with (V_a + V_b)
// It accepts the amount to lock as the input
func (s *swapState) lockFunds(amount *coins.PiconeroAmount) error {
	xmrtakerPublicKeys := mcrypto.NewPublicKeyPair(s.xmrtakerPublicSpendKey, s.xmrtakerPrivateViewKey.Public())
	swapDestAddr := mcrypto.SumSpendAndViewKeys(xmrtakerPublicKeys, s.pubkeys).Address(s.Env())
	log.Infof("going to lock XMR funds, amount=%s XMR", amount.AsMoneroString())

	balance, err := s.XMRClient().GetBalance(0)
	if err != nil {
		return err
	}

	log.Debug("total XMR balance: ", coins.FmtPiconeroAsXMR(balance.Balance))
	log.Info("unlocked XMR balance: ", coins.FmtPiconeroAsXMR(balance.UnlockedBalance))
	log.Infof("Starting lock of %s XMR in address %s", amount.AsMoneroString(), swapDestAddr)

	// set next expected event here, otherwise if we restart while `Transfer` is happening,
	// we won't notice that we already locked the XMR on restart.
	err = s.setNextExpectedEvent(EventContractReadyType)
	if err != nil {
		return fmt.Errorf("failed to set next expected event to EventContractReadyType: %w", err)
	}

	transfer, err := s.XMRClient().Transfer(s.ctx, swapDestAddr, 0, amount, monero.MinSpendConfirmations)
	if err != nil {
		return err
	}

	log.Infof("Successfully locked XMR funds: txID=%s address=%s block=%d",
		transfer.TxID, swapDestAddr, transfer.Height)
	return nil
}
