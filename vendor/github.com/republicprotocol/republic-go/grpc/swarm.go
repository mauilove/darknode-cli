package grpc

import (
	"errors"
	"fmt"

	"github.com/republicprotocol/republic-go/identity"
	"github.com/republicprotocol/republic-go/logger"
	"github.com/republicprotocol/republic-go/swarm"
	"golang.org/x/net/context"
)

// ErrRateLimitExceeded is returned when the same client sends more than one
// request to the server within a specified rate limit.
var ErrRateLimitExceeded = errors.New("cannot process request, rate limit exceeded")

// ErrPingRequestIsNil is returned when a gRPC ping request is nil or has nil
// fields.
var ErrPingRequestIsNil = errors.New("ping request is nil")

// ErrPongRequestIsNil is returned when a gRPC pong request is nil or has nil
// fields.
var ErrPongRequestIsNil = errors.New("pong request is nil")

// ErrQueryRequestIsNil is returned when a gRPC query request is nil or has nil
// fields.
var ErrQueryRequestIsNil = errors.New("query request is nil")

// ErrMultiAddressIsNil is returned when a multi-address is nil or has nil
// fields.
var ErrMultiAddressIsNil = errors.New("multi-address is nil")

// ErrAddressIsNil is returned when an address is nil.
var ErrAddressIsNil = errors.New("address is nil")

type swarmClient struct {
	addr  identity.Address
	store swarm.MultiAddressStorer
}

// NewSwarmClient returns an implementation of the swarm.Client interface that
// uses gRPC and a recycled connection pool.
func NewSwarmClient(store swarm.MultiAddressStorer, addr identity.Address) swarm.Client {
	return &swarmClient{
		addr:  addr,
		store: store,
	}
}

// Ping implements the swarm.Client interface.
func (client *swarmClient) Ping(ctx context.Context, to identity.MultiAddress, multiAddr identity.MultiAddress) error {
	if multiAddr.IsNil() {
		return ErrMultiAddressIsNil
	}
	conn, err := Dial(ctx, to)
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot dial %v: %v", to, err))
		return fmt.Errorf("cannot dial %v: %v", to, err)
	}
	defer conn.Close()

	request := &PingRequest{
		MultiAddress: &MultiAddress{
			Signature:         multiAddr.Signature,
			MultiAddress:      multiAddr.String(),
			MultiAddressNonce: multiAddr.Nonce,
		},
	}

	return Backoff(ctx, func() error {
		_, err = NewSwarmServiceClient(conn).Ping(ctx, request)
		return err
	})
}

func (client *swarmClient) Pong(ctx context.Context, to identity.MultiAddress) error {
	conn, err := Dial(ctx, to)
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot dial %v: %v", to, err))
		return fmt.Errorf("cannot dial %v: %v", to, err)
	}
	defer conn.Close()

	multiAddr, err := client.store.MultiAddress(client.addr)
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot get self details: %v", err))
		return fmt.Errorf("cannot get self details: %v", err)
	}

	request := &PongRequest{
		MultiAddress: &MultiAddress{
			Signature:         multiAddr.Signature,
			MultiAddress:      multiAddr.String(),
			MultiAddressNonce: multiAddr.Nonce,
		},
	}

	return Backoff(ctx, func() error {
		_, err = NewSwarmServiceClient(conn).Pong(ctx, request)
		return err
	})
}

// Query implements the swarm.Client interface.
func (client *swarmClient) Query(ctx context.Context, to identity.MultiAddress, query identity.Address) (identity.MultiAddresses, error) {
	if query == "" {
		return identity.MultiAddresses{}, ErrAddressIsNil
	}
	conn, err := Dial(ctx, to)
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot dial %v: %v", to, err))
		return identity.MultiAddresses{}, fmt.Errorf("cannot dial %v: %v", to, err)
	}
	defer conn.Close()

	request := &QueryRequest{
		Address: query.String(),
	}

	var response *QueryResponse
	if err := Backoff(ctx, func() error {
		response, err = NewSwarmServiceClient(conn).Query(ctx, request)
		return err
	}); err != nil {
		return identity.MultiAddresses{}, err
	}

	multiAddrs := identity.MultiAddresses{}
	for _, multiAddrMsg := range response.MultiAddresses {
		multiAddr, err := identity.NewMultiAddressFromString(multiAddrMsg.MultiAddress)
		if err != nil {
			logger.Network(logger.LevelWarn, fmt.Sprintf("cannot parse %v: %v", multiAddrMsg.MultiAddress, err))
			continue
		}
		multiAddr.Nonce = multiAddrMsg.MultiAddressNonce
		multiAddr.Signature = multiAddrMsg.Signature
		multiAddrs = append(multiAddrs, multiAddr)
	}
	return multiAddrs, nil
}

// MultiAddress implements the swarm.Client interface.
func (client *swarmClient) MultiAddress() identity.MultiAddress {
	multiAddr, err := client.store.MultiAddress(client.addr)
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot retrieve own multiaddress: %v", err))
		return identity.MultiAddress{}
	}
	return multiAddr
}

// SwarmService is a Service that implements the gRPC SwarmService defined in
// protobuf. It delegates responsibility for handling the Ping and Query RPCs
// to a swarm.Server.
type SwarmService struct {
	server swarm.Server
}

// NewSwarmService returns a SwarmService that uses the swarm.Server as a
// delegate.
func NewSwarmService(server swarm.Server) SwarmService {
	return SwarmService{
		server: server,
	}
}

// Register implements the Service interface.
func (service *SwarmService) Register(server *Server) {
	if server == nil {
		logger.Network(logger.LevelError, "server is nil")
		return
	}
	RegisterSwarmServiceServer(server.Server, service)
}

// Ping is an RPC used to notify a SwarmService about the existence of a
// client. In the PingRequest, the client sends a signed identity.MultiAddress
// and the SwarmService delegates the responsibility of handling this signed
// identity.MultiAddress to its swarm.Server. If its swarm.Server accepts the
// signed identity.MultiAddress of the client it will return its own signed
// identity.MultiAddress in a PingResponse.
func (service *SwarmService) Ping(ctx context.Context, request *PingRequest) (*PingResponse, error) {
	// Check for empty or invalid request fields.
	if request == nil {
		return nil, ErrPingRequestIsNil
	}
	if request.MultiAddress == nil {
		return nil, ErrMultiAddressIsNil
	}
	from, err := identity.NewMultiAddressFromString(request.GetMultiAddress().GetMultiAddress())
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot unmarshal multiaddress: %v", err))
		return nil, fmt.Errorf("cannot unmarshal multiaddress: %v", err)
	}
	from.Signature = request.GetMultiAddress().GetSignature()
	from.Nonce = request.GetMultiAddress().GetMultiAddressNonce()

	err = service.server.Ping(ctx, from)
	if err != nil {
		logger.Network(logger.LevelInfo, fmt.Sprintf("cannot update store with: %v", err))
		return &PingResponse{}, fmt.Errorf("cannot update store: %v", err)
	}
	return &PingResponse{}, nil
}

// Pong is an RPC used to notify a SwarmService about the existence of a
// client. In the PingRequest, the client sends a signed identity.MultiAddress
// and the SwarmService delegates the responsibility of handling this signed
// identity.MultiAddress to its swarm.Server. If its swarm.Server accepts the
// signed identity.MultiAddress of the client it will return its own signed
// identity.MultiAddress in a PongResponse.
func (service *SwarmService) Pong(ctx context.Context, request *PongRequest) (*PongResponse, error) {
	// Check for empty or invalid request fields.
	if request == nil {
		return nil, ErrPongRequestIsNil
	}
	if request.MultiAddress == nil {
		return nil, ErrMultiAddressIsNil
	}
	from, err := identity.NewMultiAddressFromString(request.GetMultiAddress().GetMultiAddress())
	if err != nil {
		logger.Network(logger.LevelError, fmt.Sprintf("cannot unmarshal multiaddress: %v", err))
		return nil, fmt.Errorf("cannot unmarshal multiaddress: %v", err)
	}

	from.Signature = request.GetMultiAddress().GetSignature()
	from.Nonce = request.GetMultiAddress().GetMultiAddressNonce()

	err = service.server.Pong(ctx, from)
	if err != nil {
		logger.Network(logger.LevelInfo, fmt.Sprintf("cannot update storer with %v: %v", request.GetMultiAddress(), err))
		return &PongResponse{}, fmt.Errorf("cannot update storer: %v", err)
	}
	return &PongResponse{}, nil
}

// Query is an RPC used to find identity.MultiAddresses. In the QueryRequest,
// the client sends an identity.Address and the SwarmService will stream
// identity.MultiAddresses to the client. The SwarmService delegates
// responsibility to its swarm.Server to return identity.MultiAddresses that
// are close to the queried identity.Address.
func (service *SwarmService) Query(ctx context.Context, request *QueryRequest) (*QueryResponse, error) {
	// Check for empty or invalid request fields.
	if request == nil {
		return nil, ErrQueryRequestIsNil
	}
	if request.Address == "" {
		return nil, ErrAddressIsNil
	}
	query := identity.Address(request.GetAddress())
	multiAddrs, err := service.server.Query(ctx, query)
	if err != nil {
		return nil, err
	}

	multiAddrMsgs := make([]*MultiAddress, len(multiAddrs))
	for i, multiAddr := range multiAddrs {
		multiAddrMsgs[i] = &MultiAddress{
			MultiAddress:      multiAddr.String(),
			Signature:         multiAddr.Signature,
			MultiAddressNonce: multiAddr.Nonce,
		}
	}

	return &QueryResponse{
		MultiAddresses: multiAddrMsgs,
	}, nil
}
