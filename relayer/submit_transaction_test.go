// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

package relayer

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/athanorlabs/atomic-swap/common/types"
	"github.com/athanorlabs/atomic-swap/dleq"
	contracts "github.com/athanorlabs/atomic-swap/ethereum"
	"github.com/athanorlabs/atomic-swap/ethereum/block"
	"github.com/athanorlabs/atomic-swap/ethereum/extethclient"
	"github.com/athanorlabs/atomic-swap/tests"
)

func Test_ValidateAndSendTransaction(t *testing.T) {
	sk := tests.GetMakerTestKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ec := extethclient.CreateTestClient(t, sk)
	txOpts, err := ec.TxOpts(ctx)
	require.NoError(t, err)

	// generate claim secret and public key
	dleq := &dleq.DefaultDLEq{}
	proof, err := dleq.Prove()
	require.NoError(t, err)
	res, err := dleq.Verify(proof)
	require.NoError(t, err)

	// hash public key of claim secret
	cmt := res.Secp256k1PublicKey().Keccak256()

	pub := sk.Public().(*ecdsa.PublicKey)
	addr := crypto.PubkeyToAddress(*pub)

	swapCreatorAddr, forwarderAddr := deployContracts(t, ec.Raw(), sk)

	swapCreator, err := contracts.NewSwapCreator(swapCreatorAddr, ec.Raw())
	require.NoError(t, err)

	testT0Timeout := big.NewInt(300) // 5 minutes
	testT1Timeout := testT0Timeout

	value := big.NewInt(9e16)
	nonce := big.NewInt(0)
	txOpts.Value = value

	refundKey := [32]byte{1}
	tx, err := swapCreator.NewSwap(txOpts, cmt, refundKey, addr,
		testT0Timeout, testT1Timeout, types.EthAssetETH.Address(), value, nonce)
	require.NoError(t, err)
	receipt, err := block.WaitForReceipt(ctx, ec.Raw(), tx.Hash())
	require.NoError(t, err)
	require.GreaterOrEqual(t, contracts.MaxNewSwapETHGas, int(receipt.GasUsed))
	txOpts.Value = big.NewInt(0)

	logIndex := 0 // change to 2 for ERC20, but ERC20 swaps cannot use the relayer
	require.Equal(t, logIndex+1, len(receipt.Logs))
	id, err := contracts.GetIDFromLog(receipt.Logs[logIndex])
	require.NoError(t, err)

	t0, t1, err := contracts.GetTimeoutsFromLog(receipt.Logs[logIndex])
	require.NoError(t, err)

	swap := &contracts.SwapCreatorSwap{
		Owner:        addr,
		Claimer:      addr,
		PubKeyClaim:  cmt,
		PubKeyRefund: refundKey,
		Timeout0:     t0,
		Timeout1:     t1,
		Asset:        types.EthAssetETH.Address(),
		Value:        value,
		Nonce:        nonce,
	}

	// set contract to Ready
	tx, err = swapCreator.SetReady(txOpts, *swap)
	require.NoError(t, err)
	receipt, err = block.WaitForReceipt(ctx, ec.Raw(), tx.Hash())
	require.NoError(t, err)
	require.GreaterOrEqual(t, contracts.MaxSetReadyGas, int(receipt.GasUsed))

	secret := proof.Secret()

	// now let's try to claim
	req, err := CreateRelayClaimRequest(ctx, sk, ec.Raw(), swapCreatorAddr, forwarderAddr, swap, &secret)
	require.NoError(t, err)

	resp, err := ValidateAndSendTransaction(ctx, req, ec, swapCreatorAddr)
	require.NoError(t, err)

	receipt, err = block.WaitForReceipt(ctx, ec.Raw(), resp.TxHash)
	require.NoError(t, err)
	t.Logf("gas cost to call Claim via relayer: %d", receipt.GasUsed)

	// expected 1 Claimed log (ERC20 swaps have 3, but we don't support relaying with ERC20 swaps)
	require.Equal(t, 1, len(receipt.Logs))

	stage, err := swapCreator.Swaps(nil, id)
	require.NoError(t, err)
	require.Equal(t, contracts.StageCompleted, stage)

	// Now lets try to claim a second time and verify that we fail on the simulated
	// execution.
	req, err = CreateRelayClaimRequest(ctx, sk, ec.Raw(), swapCreatorAddr, forwarderAddr, swap, &secret)
	require.NoError(t, err)

	_, err = ValidateAndSendTransaction(ctx, req, ec, swapCreatorAddr)
	require.ErrorContains(t, err, "relayed transaction failed on simulation")
}
