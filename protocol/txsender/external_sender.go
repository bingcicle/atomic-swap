// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

package txsender

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/cockroachdb/apd/v3"

	"github.com/athanorlabs/atomic-swap/coins"
	"github.com/athanorlabs/atomic-swap/common"
	"github.com/athanorlabs/atomic-swap/common/types"
	contracts "github.com/athanorlabs/atomic-swap/ethereum"
	"github.com/athanorlabs/atomic-swap/ethereum/block"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	errTransactionTimeout = errors.New("timed out waiting for transaction to be signed")
	transactionTimeout    = time.Minute * 2 // amount of time user has to sign message
)

// Transaction represents a transaction to be signed by the front-end
type Transaction struct {
	To    ethcommon.Address
	Data  []byte
	Value *apd.Decimal // ETH (or ETH asset), not WEI
}

// ExternalSender represents a transaction signer and sender that is external to the daemon (ie. a front-end)
type ExternalSender struct {
	ctx          context.Context
	ec           *ethclient.Client
	abi          *abi.ABI
	contractAddr ethcommon.Address
	erc20Addr    ethcommon.Address

	sync.Mutex

	// outgoing encoded txs to be signed
	out chan *Transaction
	// incoming tx hashes
	in chan ethcommon.Hash
}

// NewExternalSender returns a new ExternalSender
func NewExternalSender(
	ctx context.Context,
	env common.Environment,
	ec *ethclient.Client,
	contractAddr ethcommon.Address,
	erc20Addr ethcommon.Address,
) (*ExternalSender, error) {
	switch env {
	case common.Mainnet, common.Stagenet:
		transactionTimeout = time.Hour
	}

	return &ExternalSender{
		ctx:          ctx,
		ec:           ec,
		abi:          contracts.SwapCreatorParsedABI,
		contractAddr: contractAddr,
		erc20Addr:    erc20Addr,
		out:          make(chan *Transaction),
		in:           make(chan ethcommon.Hash),
	}, nil
}

// SetSwapCreator sets the bound contract for the SwapCreator
func (s *ExternalSender) SetSwapCreator(_ *contracts.SwapCreator) {}

// SetSwapCreatorAddr sets the address of the SwapCreator contract
func (s *ExternalSender) SetSwapCreatorAddr(addr ethcommon.Address) {
	s.contractAddr = addr
}

// OngoingCh returns the channel of outgoing transactions to be signed and submitted
func (s *ExternalSender) OngoingCh(id types.Hash) <-chan *Transaction {
	return s.out
}

// IncomingCh returns the channel of incoming transaction hashes that have been signed and submitted
func (s *ExternalSender) IncomingCh(id types.Hash) chan<- ethcommon.Hash {
	return s.in
}

// approve prompts the external sender to sign an ERC20 approve transaction
//
//nolint:unused // not used because external sender's NewSwap doesn't support ERC20 tokens
func (s *ExternalSender) approve(
	spender ethcommon.Address,
	amount *big.Int,
) (*ethtypes.Receipt, error) {
	input, err := s.abi.Pack("approve", spender, amount)
	if err != nil {
		return nil, err
	}

	return s.sendAndReceive(input, s.erc20Addr)
}

// NewSwap prompts the external sender to sign a new_swap transaction
func (s *ExternalSender) NewSwap(
	pubKeyClaim [32]byte,
	pubKeyRefund [32]byte,
	claimer ethcommon.Address,
	timeoutDuration *big.Int,
	nonce *big.Int,
	amount coins.EthAssetAmount,
) (*ethtypes.Receipt, error) {
	// TODO: Add ERC20 token support and approve new_swap for the token transfer
	if amount.IsToken() {
		return nil, errors.New("external sender does not support ERC20 token swaps")
	}

	input, err := s.abi.Pack("new_swap", pubKeyClaim, pubKeyRefund, claimer, timeoutDuration,
		amount.TokenAddress(), amount.BigInt(), nonce)
	if err != nil {
		return nil, err
	}

	tx := &Transaction{
		To:    s.contractAddr,
		Data:  input,
		Value: amount.AsStandard(),
	}

	s.Lock()
	defer s.Unlock()

	s.out <- tx
	var txHash ethcommon.Hash
	select {
	case <-time.After(transactionTimeout):
		return nil, errTransactionTimeout
	case txHash = <-s.in:
	}

	receipt, err := block.WaitForReceipt(s.ctx, s.ec, txHash)
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// SetReady prompts the external sender to sign a set_ready transaction
func (s *ExternalSender) SetReady(swap *contracts.SwapCreatorSwap) (*ethtypes.Receipt, error) {
	input, err := s.abi.Pack("set_ready", swap)
	if err != nil {
		return nil, err
	}

	return s.sendAndReceive(input, s.contractAddr)
}

// Claim prompts the external sender to sign a claim transaction
func (s *ExternalSender) Claim(
	swap *contracts.SwapCreatorSwap,
	secret [32]byte,
) (*ethtypes.Receipt, error) {
	input, err := s.abi.Pack("claim", swap, secret)
	if err != nil {
		return nil, err
	}

	return s.sendAndReceive(input, s.contractAddr)
}

// Refund prompts the external sender to sign a refund transaction
func (s *ExternalSender) Refund(
	swap *contracts.SwapCreatorSwap,
	secret [32]byte,
) (*ethtypes.Receipt, error) {
	input, err := s.abi.Pack("refund", swap, secret)
	if err != nil {
		return nil, err
	}

	return s.sendAndReceive(input, s.contractAddr)
}

func (s *ExternalSender) sendAndReceive(input []byte, to ethcommon.Address) (*ethtypes.Receipt, error) {
	tx := &Transaction{To: to, Data: input}

	s.Lock()
	defer s.Unlock()

	s.out <- tx
	var txHash ethcommon.Hash
	select {
	case <-time.After(transactionTimeout):
		return nil, errTransactionTimeout
	case txHash = <-s.in:
	}

	receipt, err := block.WaitForReceipt(s.ctx, s.ec, txHash)
	if err != nil {
		return nil, err
	}

	return receipt, nil
}
