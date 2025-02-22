// Copyright 2023 The AthanorLabs/atomic-swap Authors
// SPDX-License-Identifier: LGPL-3.0-only

// Package rpc provides the HTTP server for incoming JSON-RPC and websocket requests to
// swapd from the local host. The answers to these queries come from 3 subsystems: net,
// personal and swap.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/MarinX/monerorpc/wallet"
	"github.com/cockroachdb/apd/v3"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/rpc/v2"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/athanorlabs/atomic-swap/coins"
	"github.com/athanorlabs/atomic-swap/common"
	"github.com/athanorlabs/atomic-swap/common/types"
	mcrypto "github.com/athanorlabs/atomic-swap/crypto/monero"
	"github.com/athanorlabs/atomic-swap/ethereum/extethclient"
	"github.com/athanorlabs/atomic-swap/protocol/swap"
	"github.com/athanorlabs/atomic-swap/protocol/txsender"
)

const (
	DaemonNamespace   = "daemon"   //nolint:revive
	DatabaseNamespace = "database" //nolint:revive
	NetNamespace      = "net"      //nolint:revive
	PersonalName      = "personal" //nolint:revive
	SwapNamespace     = "swap"     //nolint:revive
)

var log = logging.Logger("rpc")

// Server represents the JSON-RPC server
type Server struct {
	ctx        context.Context
	listener   net.Listener
	httpServer *http.Server
}

// Config ...
type Config struct {
	Ctx             context.Context
	Address         string // "IP:port"
	Net             Net
	XMRTaker        XMRTaker
	XMRMaker        XMRMaker
	ProtocolBackend ProtocolBackend
	RecoveryDB      RecoveryDB
	Namespaces      map[string]struct{}
	IsBootnodeOnly  bool
}

// AllNamespaces returns a map with all RPC namespaces set for usage in the config.
func AllNamespaces() map[string]struct{} {
	return map[string]struct{}{
		DaemonNamespace:   {},
		DatabaseNamespace: {},
		NetNamespace:      {},
		PersonalName:      {},
		SwapNamespace:     {},
	}
}

// NewServer ...
func NewServer(cfg *Config) (*Server, error) {
	rpcServer := rpc.NewServer()
	rpcServer.RegisterCodec(NewCodec(), "application/json")

	serverCtx, serverCancel := context.WithCancel(cfg.Ctx)
	err := rpcServer.RegisterService(NewDaemonService(serverCancel, cfg.ProtocolBackend), "daemon")
	if err != nil {
		return nil, err
	}

	var swapManager swap.Manager
	if cfg.ProtocolBackend != nil {
		swapManager = cfg.ProtocolBackend.SwapManager()
	}

	var netService *NetService
	for ns := range cfg.Namespaces {
		switch ns {
		case DaemonNamespace:
			continue
		case DatabaseNamespace:
			err = rpcServer.RegisterService(NewDatabaseService(cfg.RecoveryDB), DatabaseNamespace)
		case NetNamespace:
			netService = NewNetService(cfg.Net, cfg.XMRTaker, cfg.XMRMaker, swapManager, cfg.IsBootnodeOnly)
			err = rpcServer.RegisterService(netService, NetNamespace)
		case PersonalName:
			err = rpcServer.RegisterService(NewPersonalService(serverCtx, cfg.XMRMaker, cfg.ProtocolBackend), PersonalName)
		case SwapNamespace:
			err = rpcServer.RegisterService(
				NewSwapService(
					serverCtx,
					swapManager,
					cfg.XMRTaker,
					cfg.XMRMaker,
					cfg.Net,
					cfg.ProtocolBackend,
					cfg.RecoveryDB,
				),
				SwapNamespace,
			)
		default:
			err = fmt.Errorf("unknown namespace %s", ns)
		}
	}
	if err != nil {
		serverCancel()
		return nil, err
	}

	wsServer := newWsServer(serverCtx, swapManager, netService, cfg.ProtocolBackend, cfg.XMRTaker)

	lc := net.ListenConfig{}
	ln, err := lc.Listen(serverCtx, "tcp", cfg.Address)
	if err != nil {
		serverCancel()
		return nil, err
	}

	r := mux.NewRouter()
	r.Handle("/", rpcServer)
	r.Handle("/ws", wsServer)

	headersOk := handlers.AllowedHeaders([]string{"content-type", "username", "password"})
	methodsOk := handlers.AllowedMethods([]string{"GET", "HEAD", "POST", "PUT", "OPTIONS"})
	originsOk := handlers.AllowedOrigins([]string{"*"})
	server := &http.Server{
		Addr:              ln.Addr().String(),
		ReadHeaderTimeout: time.Second,
		Handler:           handlers.CORS(headersOk, methodsOk, originsOk)(r),
		BaseContext: func(listener net.Listener) context.Context {
			return serverCtx
		},
	}

	return &Server{
		ctx:        serverCtx,
		listener:   ln,
		httpServer: server,
	}, nil
}

// HttpURL returns the URL used for HTTP requests
func (s *Server) HttpURL() string { //nolint:revive
	return fmt.Sprintf("http://%s", s.httpServer.Addr)
}

// WsURL returns the URL used for websocket requests
func (s *Server) WsURL() string {
	return fmt.Sprintf("ws://%s/ws", s.httpServer.Addr)
}

// Start starts the JSON-RPC and Websocket server.
func (s *Server) Start() error {
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}

	log.Infof("Starting RPC server on %s", s.HttpURL())
	log.Infof("Starting websockets server on %s", s.WsURL())

	serverErr := make(chan error, 1)
	go func() {
		// Serve never returns nil. It returns http.ErrServerClosed if it was terminated
		// by the Shutdown.
		serverErr <- s.httpServer.Serve(s.listener)
	}()

	select {
	case <-s.ctx.Done():
		// Shutdown below is passed a closed context, which means it will shut down
		// immediately without servicing already connected clients.
		err := s.httpServer.Shutdown(s.ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Warnf("http server shutdown errored: %s", err)
		}
		// We shut down because the context was cancelled, so that's the error to return
		return s.ctx.Err()
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("RPC server failed: %s", err)
		} else {
			log.Info("RPC server shut down")
		}
		return err
	}
}

// Stop the JSON-RPC and websockets server. If server's context is not cancelled, a
// graceful shutdown happens where existing connections are serviced until disconnected.
// If the context is cancelled, the shutdown is immediate.
func (s *Server) Stop() error {
	return s.httpServer.Shutdown(s.ctx)
}

// Protocol represents the functions required by the rpc service into the protocol handler.
type Protocol interface {
	Provides() coins.ProvidesCoin
	GetOngoingSwapState(types.Hash) common.SwapState
}

// ProtocolBackend represents protocol/backend.Backend
type ProtocolBackend interface {
	Ctx() context.Context
	Env() common.Environment
	SetSwapTimeout(timeout time.Duration)
	SwapTimeout() time.Duration
	SwapManager() swap.Manager
	SwapCreatorAddr() ethcommon.Address
	SetXMRDepositAddress(*mcrypto.Address, types.Hash)
	ClearXMRDepositAddress(types.Hash)
	ETHClient() extethclient.EthClient
}

// XMRTaker ...
type XMRTaker interface {
	Protocol
	InitiateProtocol(peerID peer.ID, providesAmount *apd.Decimal, offer *types.Offer) (common.SwapState, error)
	ExternalSender(offerID types.Hash) (*txsender.ExternalSender, error)
}

// XMRMaker ...
type XMRMaker interface {
	Protocol
	MakeOffer(offer *types.Offer, useRelayer bool) (*types.OfferExtra, error)
	GetOffers() []*types.Offer
	ClearOffers([]types.Hash) error
	GetMoneroBalance() (*mcrypto.Address, *wallet.GetBalanceResponse, error)
}

// SwapManager ...
type SwapManager = swap.Manager
