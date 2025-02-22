// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

package main

import (
	"context"
	"testing"

	"github.com/athanorlabs/atomic-swap/common"
	contracts "github.com/athanorlabs/atomic-swap/ethereum"
	"github.com/athanorlabs/atomic-swap/ethereum/extethclient"
	"github.com/athanorlabs/atomic-swap/tests"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestGetOrDeploySwapCreator_DeployNoForwarder(t *testing.T) {
	pk := tests.GetTakerTestKey(t)
	ec := extethclient.CreateTestClient(t, pk)
	tmpDir := t.TempDir()

	forwarder, err := contracts.DeployGSNForwarderWithKey(context.Background(), ec.Raw(), pk)
	require.NoError(t, err)

	_, err = getOrDeploySwapCreator(
		context.Background(),
		ethcommon.Address{},
		common.Development,
		tmpDir,
		ec,
		forwarder,
	)
	require.NoError(t, err)
}

func TestGetOrDeploySwapCreator_DeployForwarderAlso(t *testing.T) {
	pk := tests.GetTakerTestKey(t)
	ec := extethclient.CreateTestClient(t, pk)
	tmpDir := t.TempDir()

	_, err := getOrDeploySwapCreator(
		context.Background(),
		ethcommon.Address{},
		common.Development,
		tmpDir,
		ec,
		ethcommon.Address{},
	)
	require.NoError(t, err)
}

func TestGetOrDeploySwapCreator_Get(t *testing.T) {
	pk := tests.GetTakerTestKey(t)
	ec := extethclient.CreateTestClient(t, pk)
	tmpDir := t.TempDir()

	forwarder, err := contracts.DeployGSNForwarderWithKey(context.Background(), ec.Raw(), pk)
	require.NoError(t, err)
	t.Log(forwarder)

	// deploy and get address
	address, err := getOrDeploySwapCreator(
		context.Background(),
		ethcommon.Address{},
		common.Development,
		tmpDir,
		ec,
		forwarder,
	)
	require.NoError(t, err)

	addr2, err := getOrDeploySwapCreator(
		context.Background(),
		address,
		common.Development,
		tmpDir,
		ec,
		ethcommon.Address{},
	)
	require.NoError(t, err)
	require.Equal(t, address, addr2)
}
