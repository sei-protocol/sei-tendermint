package p2p

import (
	"fmt"
	"sync"

	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/utils/tcp"
	"github.com/tendermint/tendermint/types"
	"github.com/tendermint/tendermint/internal/p2p/conn"
)

// MemoryNetwork is an in-memory "network" that uses buffered Go channels to
// communicate between endpoints. It is primarily meant for testing.
//
// Network endpoints are allocated via CreateTransport(), which takes a node ID,
// and the endpoint is then immediately accessible via the URL "memory:<nodeID>".
type MemoryNetwork struct {
	logger log.Logger

	mtx        sync.RWMutex
	transports map[types.NodeID]*MConnTransport
	bufferSize int
}

// NewMemoryNetwork creates a new in-memory network.
func NewMemoryNetwork(logger log.Logger, bufferSize int) *MemoryNetwork {
	return &MemoryNetwork{
		bufferSize: bufferSize,
		logger:     logger,
		transports: map[types.NodeID]*MConnTransport{},
	}
}

// CreateTransport creates a new memory transport endpoint with the given node
// ID and immediately begins listening on the address "memory:<id>". It panics
// if the node ID is already in use (which is fine, since this is for tests).
func (n *MemoryNetwork) CreateTransport(nodeID types.NodeID) *MConnTransport {
	t := TestTransport(n.logger, nodeID)

	n.mtx.Lock()
	defer n.mtx.Unlock()
	if _, ok := n.transports[nodeID]; ok {
		panic(fmt.Sprintf("memory transport with node ID %q already exists", nodeID))
	}
	n.transports[nodeID] = t
	return t
}

// GetTransport looks up a transport in the network, returning nil if not found.
func (n *MemoryNetwork) GetTransport(id types.NodeID) *MConnTransport {
	n.mtx.RLock()
	defer n.mtx.RUnlock()
	return n.transports[id]
}

// Size returns the number of transports in the network.
func (n *MemoryNetwork) Size() int {
	return len(n.transports)
}

// newMemoryTransport creates a new MemoryTransport. This is for internal use by
// MemoryNetwork, use MemoryNetwork.CreateTransport() instead.
func TestTransport(logger log.Logger, nodeID types.NodeID) *MConnTransport {
	return NewMConnTransport(
		logger.With("local", nodeID),
		Endpoint{
			Protocol: MConnProtocol,
			Addr: tcp.TestReserveAddr(),
		},
		conn.DefaultMConnConfig(),
		[]*ChannelDescriptor{},
		MConnTransportOptions{},
	)
}

