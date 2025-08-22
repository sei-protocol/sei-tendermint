package p2p

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/net/netutil"

	"github.com/tendermint/tendermint/internal/p2p/conn"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/utils/scope"
)

func validateEndpoint(endpoint Endpoint) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	if len(endpoint.IP) == 0 {
		return errors.New("endpoint has no IP address")
	}
	if endpoint.Path != "" {
		return fmt.Errorf("endpoints with path not supported (got %q)", endpoint.Path)
	}
	return nil
}

// MConnTransportOptions sets options for MConnTransport.
type MConnTransportOptions struct {
	// MaxAcceptedConnections is the maximum number of simultaneous accepted
	// (incoming) connections. Beyond this, new connections will block until
	// a slot is free. 0 means unlimited.
	MaxAcceptedConnections uint32
}

// MConnTransport is a Transport implementation using the current multiplexed
// Tendermint protocol ("MConn").
type MConnTransport struct {
	logger       log.Logger
	endpoint Endpoint
	options      MConnTransportOptions
	mConnConfig  conn.MConnConfig
	channelDescs []*ChannelDescriptor
	channelQueues   map[ChannelID]queue // inbound messages from all peers to a single channel
	channelMessages map[ChannelID]proto.Message

	closeOnce sync.Once
	listener  chan *conn.MConnection
  // Connection register here.
}

// NewMConnTransport sets up a new MConnection transport. This uses the
// proprietary Tendermint MConnection protocol, which is implemented as
// conn.MConnection.
func NewMConnTransport(
	logger log.Logger,
	mConnConfig conn.MConnConfig,
	channelDescs []*ChannelDescriptor,
	options MConnTransportOptions,
	endpoint Endpoint,
) *MConnTransport {
	return &MConnTransport{
		logger:       logger,
		options:      options,
		mConnConfig:  mConnConfig,
		channelDescs: channelDescs,
	}
}

// String implements Transport.
func (m *MConnTransport) String() string {
	return "mconn"
}

// Endpoint implements Transport.
func (m *MConnTransport) Endpoint() Endpoint {
	return m.endpoint
}

// Listen asynchronously listens for inbound connections on the given endpoint.
// It must be called exactly once before calling Accept(), and the caller must
// call Close() to shut down the listener.
func (m *MConnTransport) Run(ctx context.Context) error {
	if err := validateEndpoint(m.endpoint); err != nil {
		return err
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(
		m.endpoint.IP.String(), strconv.Itoa(int(m.endpoint.Port))))
	if err != nil {
		return fmt.Errorf("net.Listen(): %w",err)
	}
	if m.options.MaxAcceptedConnections > 0 {
		// TODO(gprusak): this does NOT set backlog size.
		// It just limits the number of concurrent connections
		listener = netutil.LimitListener(listener, int(m.options.MaxAcceptedConnections))
	}

	return scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		s.Spawn(func() error {
			<-ctx.Done()
			listener.Close()
			return nil
		})
		for {
			tcpConn,err := listener.Accept()
			if err!=nil {
				return fmt.Errorf("listener.Accept(): %w",err)
			}
			mconn := conn.NewMConnection(m.logger, tcpConn, m.mConnConfig, m.channelDescs)
			s.Spawn(func() error {
				if err := mconn.Run(ctx); err!=nil {
					m.logger.Info("conn.Run(): %w",err)
				}
				return nil
			})
			m.listener <- mconn
		}
	})
}

// Dial implements Transport.
func (m *MConnTransport) Dial(ctx context.Context, endpoint Endpoint) (Connection, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if endpoint.Port == 0 {
		endpoint.Port = 26657
	}
	dialer := net.Dialer{}
	tcpConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(
		endpoint.IP.String(), strconv.Itoa(int(endpoint.Port))))
	if err != nil {
		return nil,fmt.Errorf("dialerContext(): %w",err)
	}
	// TODO: run the connection
	return conn.NewMConnection(m.logger, tcpConn, m.mConnConfig, m.channelDescs), nil
}

// OpenChannel opens a new channel for the given message type. The caller must
// close the channel when done, before stopping the Router. messageType is the
// type of message passed through the channel (used for unmarshaling), which can
// implement Wrapper to automatically (un)wrap multiple message types in a
// wrapper message. The caller may provide a size to make the channel buffered,
// which internally makes the inbound, outbound, and error channel buffered.
func (r *Router) OpenChannel(ctx context.Context, chDesc *ChannelDescriptor) (*Channel, error) {
	r.channelMtx.Lock()
	defer r.channelMtx.Unlock()

	id := chDesc.ID
	if _, ok := r.channelQueues[id]; ok {
		return nil, fmt.Errorf("channel %v already exists", id)
	}
	r.chDescs = append(r.chDescs, chDesc)

	messageType := chDesc.MessageType

	queue := r.queueFactory(chDesc.RecvBufferCapacity)
	channel := NewChannel(id, queue.dequeue())
	channel.name = chDesc.Name

	var wrapper Wrapper
	if w, ok := messageType.(Wrapper); ok {
		wrapper = w
	}

	r.channelQueues[id] = queue
	r.channelMessages[id] = messageType

	// add the channel to the nodeInfo if it's not already there.
	r.nodeInfoProducer().AddChannel(uint16(chDesc.ID))

	r.transport.AddChannelDescriptors([]*ChannelDescriptor{chDesc})

	// Task routing channels.
	r.routeChannel(ctx, id, outCh, errCh, wrapper)

	return channel, nil
}

func (r *Router) createQueueFactory(ctx context.Context) (func(int) queue, error) {
	switch r.options.QueueType {
	case queueTypeFifo:
		return newFIFOQueue, nil

	case queueTypePriority:
		return func(size int) queue {
			if size%2 != 0 {
				size++
			}

			q := newPQScheduler(r.logger, r.metrics, r.lc, r.chDescs, uint(size)/2, uint(size)/2, defaultCapacity)
			q.start(ctx)
			return q
		}, nil

	case queueTypeSimplePriority:
		return func(size int) queue { return newSimplePriorityQueue(ctx, size, r.chDescs) }, nil

	default:
		return nil, fmt.Errorf("cannot construct queue of type %q", r.options.QueueType)
	}
}


