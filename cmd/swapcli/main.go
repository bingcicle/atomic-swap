// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

// Package main provides the entrypoint of swapcli, an executable for interacting with a
// local swapd instance from the command line.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/apd/v3"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/skip2/go-qrcode"
	"github.com/urfave/cli/v2"

	"github.com/athanorlabs/atomic-swap/cliutil"
	"github.com/athanorlabs/atomic-swap/coins"
	"github.com/athanorlabs/atomic-swap/common"
	"github.com/athanorlabs/atomic-swap/common/rpctypes"
	"github.com/athanorlabs/atomic-swap/common/types"
	"github.com/athanorlabs/atomic-swap/net"
	"github.com/athanorlabs/atomic-swap/rpcclient"
	"github.com/athanorlabs/atomic-swap/rpcclient/wsclient"
)

const (
	defaultDiscoverSearchTimeSecs = 12

	flagSwapdPort      = "swapd-port"
	flagMinAmount      = "min-amount"
	flagMaxAmount      = "max-amount"
	flagPeerID         = "peer-id"
	flagOfferID        = "offer-id"
	flagOfferIDs       = "offer-ids"
	flagExchangeRate   = "exchange-rate"
	flagProvides       = "provides"
	flagProvidesAmount = "provides-amount"
	flagUseRelayer     = "use-relayer"
	flagSearchTime     = "search-time"
	flagToken          = "token"
	flagDetached       = "detached"
)

func cliApp() *cli.App {
	return &cli.App{
		Name:                 "swapcli",
		Usage:                "Client for swapd",
		Version:              cliutil.GetVersion(),
		EnableBashCompletion: true,
		Suggest:              true,
		Commands: []*cli.Command{
			{
				Name:    "addresses",
				Aliases: []string{"a"},
				Usage:   "List our daemon's libp2p listening addresses",
				Action:  runAddresses,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:    "peers",
				Aliases: []string{"p"},
				Usage:   "List peers that are currently connected",
				Action:  runPeers,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:    "balances",
				Aliases: []string{"b"},
				Usage:   "Show our Monero and Ethereum account balances",
				Action:  runBalances,
				Flags: []cli.Flag{
					swapdPortFlag,
					&cli.StringSliceFlag{
						Name:    flagToken,
						Aliases: []string{"t"},
						EnvVars: []string{"SWAPCLI_TOKENS"},
						Usage:   "Token address to include in the balance response",
					},
				},
			},
			{
				Name:   "eth-address",
				Usage:  "Show our Ethereum address with its QR code",
				Action: runETHAddress,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:   "xmr-address",
				Usage:  "Show our Monero address with its QR code",
				Action: runXMRAddress,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:    "discover",
				Aliases: []string{"d"},
				Usage:   "Discover peers who provide Monero or relayer services",
				Action:  runDiscover,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: flagProvides,
						Usage: fmt.Sprintf("Search for %q or %q providers",
							coins.ProvidesXMR, net.RelayerProvidesStr),
						Value: string(coins.ProvidesXMR),
					},
					&cli.Uint64Flag{
						Name:  flagSearchTime,
						Usage: "Duration of time to search for, in seconds",
						Value: defaultDiscoverSearchTimeSecs,
					},
					swapdPortFlag,
				},
			},
			{
				Name:    "query",
				Aliases: []string{"q"},
				Usage:   "Query a peer for details on what they provide",
				Action:  runQuery,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     flagPeerID,
						Usage:    "Peer's ID, as provided by discover",
						Required: true,
					},
					swapdPortFlag,
				},
			},
			{
				Name:    "query-all",
				Aliases: []string{"qall"},
				Usage:   "Discover peers that provide Monero and their offers",
				Action:  runQueryAll,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: flagProvides,
						Usage: fmt.Sprintf("Coin to find providers for: one of [%s, %s]",
							coins.ProvidesXMR, coins.ProvidesETH),
						Value: string(coins.ProvidesXMR),
					},
					&cli.Uint64Flag{
						Name:  flagSearchTime,
						Usage: "Duration of time to search for, in seconds",
						Value: defaultDiscoverSearchTimeSecs,
					},
					swapdPortFlag,
				},
			},
			{
				Name:    "make",
				Aliases: []string{"m"},
				Usage:   "Make a swap offer; currently Monero holders must be the makers",
				Action:  runMake,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     flagMinAmount,
						Usage:    "Minimum amount to be swapped, in XMR",
						Required: true,
					},
					&cli.StringFlag{
						Name:     flagMaxAmount,
						Usage:    "Maximum amount to be swapped, in XMR",
						Required: true,
					},
					&cli.StringFlag{
						Name:     flagExchangeRate,
						Usage:    "Desired exchange rate of XMR:ETH, eg. --exchange-rate=0.1 means 10XMR = 1ETH",
						Required: true,
					},
					&cli.BoolFlag{
						Name:  flagDetached,
						Usage: "Exit immediately instead of subscribing to notifications about the swap's status",
					},
					&cli.StringFlag{
						Name:  flagToken,
						Usage: "Use to pass the ethereum ERC20 token address to receive instead of ETH",
					},
					&cli.BoolFlag{
						Name:  flagUseRelayer,
						Usage: "Use the relayer even if the receiving account has enough ETH to claim",
					},
					swapdPortFlag,
				},
			},
			{
				Name:    "take",
				Aliases: []string{"t"},
				Usage:   "Initiate a swap by taking an offer; currently only ETH holders can be the takers",
				Action:  runTake,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     flagPeerID,
						Usage:    "Peer's ID, as provided by discover",
						Required: true,
					},
					&cli.StringFlag{
						Name:     flagOfferID,
						Usage:    "ID of the offer being taken",
						Required: true,
					},
					&cli.StringFlag{
						Name:     flagProvidesAmount,
						Usage:    "Amount of coin to send in the swap",
						Required: true,
					},
					&cli.BoolFlag{
						Name:  flagDetached,
						Usage: "Exit immediately instead of subscribing to notifications about the swap's status",
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "ongoing",
				Usage:  "Get information about ongoing swap(s).",
				Action: runGetOngoingSwap,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  flagOfferID,
						Usage: "ID of swap to retrieve info for",
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "past",
				Usage:  "Get information about past swap(s)",
				Action: runGetPastSwap,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  flagOfferID,
						Usage: "ID of swap to retrieve info for",
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "cancel",
				Usage:  "Cancel a ongoing swap if possible. Depending on the swap stage, this may not be possible.",
				Action: runCancel,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  flagOfferID,
						Usage: "ID of swap to retrieve info for",
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "clear-offers",
				Usage:  "Clear current offers. If no offer IDs are provided, clears all current offers.",
				Action: runClearOffers,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  flagOfferIDs,
						Usage: "A comma-separated list of offer IDs to delete",
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "get-offers",
				Usage:  "Get all current offers.",
				Action: runGetOffers,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:   "get-status",
				Usage:  "Get the status of a current swap.",
				Action: runGetStatus,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     flagOfferID,
						Usage:    "ID of swap to retrieve info for",
						Required: true,
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "set-swap-timeout",
				Usage:  "Set the duration between swap initiation and t0 and t0 and t1, in seconds",
				Action: runSetSwapTimeout,
				Flags: []cli.Flag{
					&cli.UintFlag{
						Name:     "duration",
						Usage:    "Duration of timeout, in seconds",
						Required: true,
					},
					swapdPortFlag,
				},
			},
			{
				Name:   "suggested-exchange-rate",
				Usage:  "Returns the current mainnet exchange rate based on ETH/USD and XMR/USD price feeds.",
				Action: runSuggestedExchangeRate,
				Flags:  []cli.Flag{swapdPortFlag},
			},
			{
				Name:   "get-swap-timeout",
				Usage:  "Get the duration between swap initiation and t0 and t0 and t1, in seconds",
				Action: runGetSwapTimeout,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:   "version",
				Usage:  "Get the client and server versions",
				Action: runGetVersions,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:   "shutdown",
				Usage:  "Shutdown swapd",
				Action: runShutdown,
				Flags: []cli.Flag{
					swapdPortFlag,
				},
			},
			{
				Name:  "recovery",
				Usage: "Methods that should only be used as a last resort in the case of an unrecoverable swap error.",
				Subcommands: []*cli.Command{
					{
						Name: "get-contract-swap-info",
						Usage: "Get information about a swap needed to call the contract functions.\n" +
							"Returns the contract address, the swap's struct as represented in the contract,\n" +
							"and the hash of the swap's struct, which is used as its contract identifier.\n" +
							"Note: this is only useful if you plan to manually call the contract functions.",
						Action: runGetContractSwapInfo,
						Flags: []cli.Flag{
							&cli.StringFlag{
								Name:     flagOfferID,
								Usage:    "ID of swap for which query for",
								Required: true,
							},
							swapdPortFlag,
						},
					},
					{
						Name: "get-swap-secret",
						Usage: "Get the secret for a swap.\n" +
							"WARNING: do NOT share this secret with anyone. Doing so may result in a loss of funds.\n" +
							"You should not use this function unless you are sure of what you're doing.\n" +
							"This function is only useful if you plan to try to manually recover funds.",
						Action: runGetSwapSecret,
						Flags: []cli.Flag{
							&cli.StringFlag{
								Name:     flagOfferID,
								Usage:    "ID of swap for which to get the secret for",
								Required: true,
							},
							swapdPortFlag,
						},
					},
					{
						Name: "claim",
						Usage: "Manually call claim() in the contract for a given swap.\n" +
							"WARNING: This should only be used as a last resort if the normal swap process fails\n" +
							"and restarting the node does not resolve the issue.",
						Action: runClaim,
						Flags: []cli.Flag{
							&cli.StringFlag{
								Name:     flagOfferID,
								Usage:    "ID of swap for which to call claim()",
								Required: true,
							},
							swapdPortFlag,
						},
					},
					{
						Name: "refund",
						Usage: "Manually call refund() in the contract for a given swap.\n" +
							"WARNING: This should only be used as a last resort if the normal swap process fails\n" +
							"and restarting the node does not resolve the issue.",
						Action: runRefund,
						Flags: []cli.Flag{
							&cli.StringFlag{
								Name:     flagOfferID,
								Usage:    "ID of swap for which to call refund()",
								Required: true,
							},
							swapdPortFlag,
						},
					},
				},
			},
		},
	}
}

var (
	swapdPortFlag = &cli.UintFlag{
		Name:    flagSwapdPort,
		Aliases: []string{"p"},
		Usage:   "RPC port of swap daemon",
		Value:   common.DefaultSwapdPort,
		EnvVars: []string{"SWAPD_PORT"},
	}
)

func main() {
	if err := cliApp().Run(os.Args); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

func newRRPClient(ctx *cli.Context) *rpcclient.Client {
	swapdPort := ctx.Uint(flagSwapdPort)
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", swapdPort)
	return rpcclient.NewClient(ctx.Context, endpoint)
}

func newWSClient(ctx *cli.Context) (wsclient.WsClient, error) {
	swapdPort := ctx.Uint(flagSwapdPort)
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/ws", swapdPort)
	return wsclient.NewWsClient(ctx.Context, endpoint)
}

func runAddresses(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	resp, err := c.Addresses()
	if err != nil {
		return err
	}

	fmt.Println("Local listening multi-addresses:")
	for i, a := range resp.Addrs {
		fmt.Printf("%d: %s\n", i+1, a)
	}
	if len(resp.Addrs) == 0 {
		fmt.Println("[none]")
	}
	return nil
}

func runPeers(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	resp, err := c.Peers()
	if err != nil {
		return err
	}

	fmt.Println("Connected peer multi-addresses:")
	for i, a := range resp.Addrs {
		fmt.Printf("%d: %s\n", i+1, a)
	}
	if len(resp.Addrs) == 0 {
		fmt.Println("[none]")
	}
	return nil
}

func runBalances(ctx *cli.Context) error {
	c := newRRPClient(ctx)

	request := &rpctypes.BalancesRequest{}
	tokens := ctx.StringSlice(flagToken)
	for _, tokenAddr := range tokens {
		if !ethcommon.IsHexAddress(tokenAddr) {
			return fmt.Errorf("invalid token address: %q", tokenAddr)
		}
		request.TokenAddrs = append(request.TokenAddrs, ethcommon.HexToAddress(tokenAddr))
	}

	balances, err := c.Balances(request)
	if err != nil {
		return err
	}

	fmt.Printf("Ethereum address: %s\n", balances.EthAddress)
	fmt.Printf("ETH Balance: %s\n", balances.WeiBalance.AsEtherString())
	fmt.Println()

	for _, tokenBalance := range balances.TokenBalances {
		fmt.Printf("Token: %s\n", tokenBalance.TokenInfo.Address)
		fmt.Printf("Name: %q\n", tokenBalance.TokenInfo.Name)
		fmt.Printf("Symbol: %q\n", tokenBalance.TokenInfo.Symbol)
		fmt.Printf("Balance: %s\n", tokenBalance.AsStandard().Text('f'))
		fmt.Println()
	}

	fmt.Printf("Monero address: %s\n", balances.MoneroAddress)
	fmt.Printf("XMR Balance: %s\n", balances.PiconeroBalance.AsMoneroString())
	fmt.Printf("Unlocked XMR balance: %s\n",
		balances.PiconeroUnlockedBalance.AsMoneroString())
	fmt.Printf("Blocks to unlock: %d\n", balances.BlocksToUnlock)
	return nil
}

func runETHAddress(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	balances, err := c.Balances(nil)
	if err != nil {
		return err
	}
	fmt.Printf("Ethereum address: %s\n", balances.EthAddress)
	code, err := qrcode.New(balances.EthAddress.String(), qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Println(code.ToString(false))
	return nil
}

func runXMRAddress(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	balances, err := c.Balances(nil)
	if err != nil {
		return err
	}
	fmt.Printf("Monero address: %s\n", balances.MoneroAddress)
	code, err := qrcode.New(balances.MoneroAddress.String(), qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Println(code.ToString(true))
	return nil
}

func runDiscover(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	provides := ctx.String(flagProvides)
	peerIDs, err := c.Discover(provides, ctx.Uint64(flagSearchTime))
	if err != nil {
		return err
	}

	for i, peerID := range peerIDs {
		fmt.Printf("Peer %d: %v\n", i, peerID)
	}
	if len(peerIDs) == 0 {
		fmt.Println("[none]")
	}

	return nil
}

func runQuery(ctx *cli.Context) error {
	peerID, err := peer.Decode(ctx.String(flagPeerID))
	if err != nil {
		return errInvalidFlagValue(flagPeerID, err)
	}

	c := newRRPClient(ctx)
	res, err := c.Query(peerID)
	if err != nil {
		return err
	}

	for i, o := range res.Offers {
		err = printOffer(c, o, i, "")
		if err != nil {
			return err
		}
	}
	return nil
}

func runQueryAll(ctx *cli.Context) error {
	provides, err := providesStrToVal(ctx.String(flagProvides))
	if err != nil {
		return err
	}

	searchTime := ctx.Uint64(flagSearchTime)

	c := newRRPClient(ctx)
	peerOffers, err := c.QueryAll(provides, searchTime)
	if err != nil {
		return err
	}

	for i, po := range peerOffers {
		if i > 0 {
			fmt.Println("---")
		}
		fmt.Printf("Peer %d:\n", i)
		fmt.Printf("  Peer ID: %v\n", po.PeerID)
		fmt.Printf("  Offers:\n")
		for j, o := range po.Offers {
			err = printOffer(c, o, j, "    ")
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func runMake(ctx *cli.Context) error {
	c := newRRPClient(ctx)

	min, err := cliutil.ReadUnsignedDecimalFlag(ctx, flagMinAmount)
	if err != nil {
		return err
	}

	max, err := cliutil.ReadUnsignedDecimalFlag(ctx, flagMaxAmount)
	if err != nil {
		return err
	}

	ethAssetStr := ctx.String(flagToken)
	ethAsset := types.EthAssetETH
	if ethAssetStr != "" {
		ethAsset = types.EthAsset(ethcommon.HexToAddress(ethAssetStr))
	}

	exchangeRateDec, err := cliutil.ReadUnsignedDecimalFlag(ctx, flagExchangeRate)
	if err != nil {
		return err
	}
	exchangeRate := coins.ToExchangeRate(exchangeRateDec)

	var otherMin, otherMax *apd.Decimal
	var symbol string

	if ethAsset.IsETH() {
		symbol = "ETH"

		if otherMin, err = exchangeRate.ToETH(min); err != nil {
			return err
		}

		if otherMax, err = exchangeRate.ToETH(max); err != nil {
			return err
		}
	} else {
		tokenInfo, err := c.TokenInfo(ethAsset.Address()) //nolint:govet
		if err != nil {
			return err
		}

		symbol = strconv.Quote(tokenInfo.Symbol)

		if otherMin, err = exchangeRate.ToERC20Amount(min, tokenInfo); err != nil {
			return err
		}

		if otherMax, err = exchangeRate.ToERC20Amount(max, tokenInfo); err != nil {
			return err
		}

	}

	printOfferSummary := func(offerResp *rpctypes.MakeOfferResponse) {
		fmt.Println("Published:")
		fmt.Printf("\tOffer ID:  %s\n", offerResp.OfferID)
		fmt.Printf("\tPeer ID:   %s\n", offerResp.PeerID)
		fmt.Printf("\tTaker Min: %s %s\n", otherMin.Text('f'), symbol)
		fmt.Printf("\tTaker Max: %s %s\n", otherMax.Text('f'), symbol)
	}

	alwaysUseRelayer := ctx.Bool(flagUseRelayer)

	if !ctx.Bool(flagDetached) {
		wsc, err := newWSClient(ctx) //nolint:govet
		if err != nil {
			return err
		}
		defer wsc.Close()

		resp, statusCh, err := wsc.MakeOfferAndSubscribe(
			min,
			max,
			exchangeRate,
			ethAsset,
			alwaysUseRelayer,
		)
		if err != nil {
			return err
		}

		printOfferSummary(resp)

		for stage := range statusCh {
			fmt.Printf("%s > Stage updated: %s\n", time.Now().Format(common.TimeFmtSecs), stage)
			if !stage.IsOngoing() {
				return nil
			}
		}

		return nil
	}

	resp, err := c.MakeOffer(min, max, exchangeRate, ethAsset, alwaysUseRelayer)
	if err != nil {
		return err
	}

	printOfferSummary(resp)
	return nil
}

func runTake(ctx *cli.Context) error {
	peerID, err := peer.Decode(ctx.String(flagPeerID))
	if err != nil {
		return errInvalidFlagValue(flagPeerID, err)
	}
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	providesAmount, err := cliutil.ReadUnsignedDecimalFlag(ctx, flagProvidesAmount)
	if err != nil {
		return err
	}

	if !ctx.Bool(flagDetached) {
		wsc, err := newWSClient(ctx)
		if err != nil {
			return err
		}
		defer wsc.Close()

		statusCh, err := wsc.TakeOfferAndSubscribe(peerID, offerID, providesAmount)
		if err != nil {
			return err
		}

		fmt.Printf("Initiated swap with offer ID %s\n", offerID)

		for stage := range statusCh {
			fmt.Printf("%s > Stage updated: %s\n", time.Now().Format(common.TimeFmtSecs), stage)
			if !stage.IsOngoing() {
				return nil
			}
		}

		return nil
	}

	c := newRRPClient(ctx)
	if err := c.TakeOffer(peerID, offerID, providesAmount); err != nil {
		return err
	}

	fmt.Printf("Initiated swap with offer ID %s\n", offerID)
	return nil
}

func runGetOngoingSwap(ctx *cli.Context) error {
	var offerID *types.Hash

	if ctx.IsSet(flagOfferID) {
		hash, err := types.HexToHash(ctx.String(flagOfferID))
		if err != nil {
			return errInvalidFlagValue(flagOfferID, err)
		}
		offerID = &hash
	}

	c := newRRPClient(ctx)
	resp, err := c.GetOngoingSwap(offerID)
	if err != nil {
		return err
	}

	fmt.Println("Ongoing swaps:")
	if len(resp.Swaps) == 0 {
		fmt.Println("[none]")
		return nil
	}

	for i, info := range resp.Swaps {
		if i > 0 {
			fmt.Printf("---\n")
		}

		providedCoin, receivedCoin, err := providedAndReceivedSymbols(c, info.Provided, info.EthAsset)
		if err != nil {
			return err
		}

		fmt.Printf("ID: %s\n", info.ID)
		fmt.Printf("Start time: %s\n", info.StartTime.Format(common.TimeFmtSecs))
		fmt.Printf("Provided: %s %s\n", info.ProvidedAmount.Text('f'), providedCoin)
		fmt.Printf("Receiving: %s %s\n", info.ExpectedAmount.Text('f'), receivedCoin)
		fmt.Printf("Exchange Rate: %s ETH/XMR\n", info.ExchangeRate)
		fmt.Printf("Status: %s\n", info.Status)
		fmt.Printf("Time status was last updated: %s\n", info.LastStatusUpdateTime.Format(common.TimeFmtSecs))
		if info.Timeout0 != nil && info.Timeout1 != nil {
			fmt.Printf("First timeout: %s\n", info.Timeout0.Format(common.TimeFmtSecs))
			fmt.Printf("Second timeout: %s\n", info.Timeout1.Format(common.TimeFmtSecs))
		}
		fmt.Printf("Estimated time to completion: %s\n", info.EstimatedTimeToCompletion)
	}

	return nil
}

func runGetPastSwap(ctx *cli.Context) error {
	var offerID *types.Hash

	if ctx.IsSet(flagOfferID) {
		hash, err := types.HexToHash(ctx.String(flagOfferID))
		if err != nil {
			return errInvalidFlagValue(flagOfferID, err)
		}
		offerID = &hash
	}

	c := newRRPClient(ctx)
	resp, err := c.GetPastSwap(offerID)
	if err != nil {
		return err
	}

	fmt.Println("Past swaps:")
	if len(resp.Swaps) == 0 {
		fmt.Println("[none]")
		return nil
	}

	for i, info := range resp.Swaps {
		if i > 0 {
			fmt.Printf("---\n")
		}

		providedCoin, receivedCoin, err := providedAndReceivedSymbols(c, info.Provided, info.EthAsset)
		if err != nil {
			return err
		}

		endTime := "-"
		if info.EndTime != nil {
			endTime = info.EndTime.Format(common.TimeFmtSecs)
		}

		fmt.Printf("ID: %s\n", info.ID)
		fmt.Printf("Start time: %s\n", info.StartTime.Format(common.TimeFmtSecs))
		fmt.Printf("End time: %s\n", endTime)
		fmt.Printf("Provided: %s %s\n", info.ProvidedAmount.Text('f'), providedCoin)
		fmt.Printf("Received: %s %s\n", info.ExpectedAmount.Text('f'), receivedCoin)
		fmt.Printf("Exchange Rate: %s ETH/XMR\n", info.ExchangeRate)
		fmt.Printf("Status: %s\n", info.Status)
	}

	return nil
}

func runCancel(ctx *cli.Context) error {
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	c := newRRPClient(ctx)
	fmt.Printf("Attempting to exit swap with id %s\n", offerID)
	resp, err := c.Cancel(offerID)
	if err != nil {
		return err
	}

	fmt.Printf("Cancelled successfully, exit status: %s\n", resp)
	return nil
}

func runClearOffers(ctx *cli.Context) error {
	c := newRRPClient(ctx)

	ids := ctx.String(flagOfferIDs)
	if ids == "" {
		err := c.ClearOffers(nil)
		if err != nil {
			return err
		}

		fmt.Printf("Cleared all offers successfully.\n")
		return nil
	}

	var offerIDs []types.Hash
	for _, offerIDStr := range strings.Split(ids, ",") {
		id, err := types.HexToHash(strings.TrimSpace(offerIDStr))
		if err != nil {
			return errInvalidFlagValue(flagOfferIDs, err)
		}
		offerIDs = append(offerIDs, id)
	}
	err := c.ClearOffers(offerIDs)
	if err != nil {
		return err
	}

	fmt.Printf("Cleared offers successfully: %s\n", ids)
	return nil
}

func runGetOffers(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	resp, err := c.GetOffers()
	if err != nil {
		return err
	}

	fmt.Println("Peer ID (self):", resp.PeerID)
	fmt.Println("Offers:")
	for i, offer := range resp.Offers {
		err = printOffer(c, offer, i, "  ")
		if err != nil {
			return err
		}
	}
	if len(resp.Offers) == 0 {
		fmt.Println("[no offers]")
	}

	return nil
}

func runGetStatus(ctx *cli.Context) error {
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	c := newRRPClient(ctx)
	resp, err := c.GetStatus(offerID)
	if err != nil {
		return err
	}

	fmt.Printf("Start time: %s\n", resp.StartTime.Format(common.TimeFmtSecs))
	fmt.Printf("Status=%s: %s\n", resp.Status, resp.Description)
	return nil
}

func runClaim(ctx *cli.Context) error {
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	c := newRRPClient(ctx)
	resp, err := c.Claim(offerID)
	if err != nil {
		return err
	}

	fmt.Printf("Transaction hash: %s\n", resp.TxHash)
	return nil
}

func runRefund(ctx *cli.Context) error {
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	c := newRRPClient(ctx)
	resp, err := c.Refund(offerID)
	if err != nil {
		return err
	}

	fmt.Printf("Transaction hash: %s\n", resp.TxHash)
	return nil
}

func runSetSwapTimeout(ctx *cli.Context) error {
	duration := ctx.Uint("duration")
	if duration == 0 {
		return errNoDuration
	}

	c := newRRPClient(ctx)
	err := c.SetSwapTimeout(uint64(duration))
	if err != nil {
		return err
	}

	fmt.Printf("Set timeout duration to %d seconds\n", duration)
	return nil
}

func runGetSwapTimeout(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	resp, err := c.GetSwapTimeout()
	if err != nil {
		return err
	}

	fmt.Printf("Swap timeout duration: %d seconds\n", resp.Timeout)
	return nil
}

func runSuggestedExchangeRate(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	resp, err := c.SuggestedExchangeRate()
	if err != nil {
		return err
	}

	fmt.Printf("Exchange rate: %s\n", resp.ExchangeRate)
	fmt.Printf("XMR/USD Price: %-13s (%s)\n", resp.XMRPrice, resp.XMRUpdatedAt)
	fmt.Printf("ETH/USD Price: %-13s (%s)\n", resp.ETHPrice, resp.ETHUpdatedAt)

	return nil
}

func runGetVersions(ctx *cli.Context) error {
	fmt.Printf("swapcli: %s\n", cliutil.GetVersion())

	c := newRRPClient(ctx)
	resp, err := c.Version()
	if err != nil {
		return err
	}

	fmt.Printf("swapd: %s\n", resp.SwapdVersion)
	fmt.Printf("p2p version: %s\n", resp.P2PVersion)
	fmt.Printf("env: %s\n", resp.Env)
	fmt.Printf("swap creator address: %s\n", resp.SwapCreatorAddr)

	return nil
}

func runShutdown(ctx *cli.Context) error {
	c := newRRPClient(ctx)
	err := c.Shutdown()
	if err != nil {
		return err
	}
	return nil
}

func runGetContractSwapInfo(ctx *cli.Context) error {
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	c := newRRPClient(ctx)
	resp, err := c.GetContractSwapInfo(offerID)
	if err != nil {
		return err
	}

	fmt.Printf("Contract address: %s\n", resp.SwapCreatorAddr)
	fmt.Printf("Block at which newSwap was called: %d\n", resp.StartNumber)
	fmt.Printf("Swap ID as stored in the contract: %s\n", resp.SwapID)
	fmt.Printf("Swap struct as stored in the contract:\n")
	fmt.Printf("\tOwner: %s\n", resp.Swap.Owner)
	fmt.Printf("\tClaimer: %s\n", resp.Swap.Claimer)
	fmt.Printf("\tPubKeyClaim: %x\n", resp.Swap.PubKeyClaim)
	fmt.Printf("\tPubKeyRefund: %x\n", resp.Swap.PubKeyRefund)
	fmt.Printf("\tTimeout0: %s\n", resp.Swap.Timeout0)
	fmt.Printf("\tTimeout1: %s\n", resp.Swap.Timeout1)
	fmt.Printf("\tAsset: %s\n", resp.Swap.Asset)
	fmt.Printf("\tValue: %s\n", resp.Swap.Value)
	fmt.Printf("\tNonce: %s\n", resp.Swap.Nonce)
	return nil
}

func runGetSwapSecret(ctx *cli.Context) error {
	offerID, err := types.HexToHash(ctx.String(flagOfferID))
	if err != nil {
		return errInvalidFlagValue(flagOfferID, err)
	}

	c := newRRPClient(ctx)
	resp, err := c.GetSwapSecret(offerID)
	if err != nil {
		return err
	}

	fmt.Printf("Swap secret: %s\n", resp.Secret.Hex())
	return nil
}

func providesStrToVal(providesStr string) (coins.ProvidesCoin, error) {
	var provides coins.ProvidesCoin

	// The provides flag value defaults to XMR, but the user can still specify the empty
	// string explicitly, which they can do to search the empty DHT namespace for all
	// peers. `NewProvidesCoin` gives an error if you pass the empty string, so we
	// special case the empty string.
	if providesStr == "" {
		return provides, nil
	}
	return coins.NewProvidesCoin(providesStr)
}
