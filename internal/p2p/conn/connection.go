package conn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"

	"github.com/tendermint/tendermint/internal/libs/flowrate"
	"github.com/tendermint/tendermint/internal/libs/protoio"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/utils"
	"github.com/tendermint/tendermint/libs/utils/scope"
	tmp2p "github.com/tendermint/tendermint/proto/tendermint/p2p"
)

const (
	// mirrors MaxPacketMsgPayloadSize from config/config.go
	defaultMaxPacketMsgPayloadSize = 1400

	numBatchPacketMsgs = 10
	minReadBufferSize  = 1024
	minWriteBufferSize = 65536
	updateStats        = 2 * time.Second

	// some of these defaults are written in the user config
	// flushThrottle, sendRate, recvRate
	// TODO: remove values present in config
	defaultFlushThrottle = 100 * time.Millisecond

	defaultSendQueueCapacity   = 1
	defaultRecvBufferCapacity  = 4096
	defaultRecvMessageCapacity = 22020096      // 21MB
	defaultSendRate            = int64(512000) // 500KB/s
	defaultRecvRate            = int64(512000) // 500KB/s
	defaultSendTimeout         = 10 * time.Second
	defaultPingInterval        = 60 * time.Second
	defaultPongTimeout         = 90 * time.Second
)

type receiveCbFunc func(ctx context.Context, chID ChannelID, msgBytes []byte)
type errorCbFunc func(context.Context, any)

/*
Each peer has one `MConnection` (multiplex connection) instance.

__multiplex__ *noun* a system or signal involving simultaneous transmission of
several messages along a single channel of communication.

Each `MConnection` handles message transmission on multiple abstract communication
`Channel`s.  Each channel has a globally unique byte id.
The byte id and the relative priorities of each `Channel` are configured upon
initialization of the connection.

There are two methods for sending messages:

	func (m MConnection) Send(chID byte, msgBytes []byte) bool {}

`Send(chID, msgBytes)` is a blocking call that waits until `msg` is
successfully queued for the channel with the given id byte `chID`, or until the
request times out.  The message `msg` is serialized using Protobuf.

Inbound message bytes are handled with an onReceive callback function.
*/

type sendQueue struct {
	ping bool
	pong bool
	flush bool
	channels map[ChannelID]*channel
}

type MConnection struct {
	logger log.Logger
	config        MConnConfig
	conn          net.Conn
	bufConnReader *bufio.Reader
	sendQueue    utils.Watch[*sendQueue]

	onReceive     receiveCbFunc

	// close conn if pong is not received in pongTimeout
	lastMsgRecv atomic.Pointer[time.Time]
}

// MConnConfig is a MConnection configuration.
type MConnConfig struct {
	SendRate int64 `mapstructure:"send_rate"`
	RecvRate int64 `mapstructure:"recv_rate"`

	// Maximum payload size
	MaxPacketMsgPayloadSize int `mapstructure:"max_packet_msg_payload_size"`

	// Interval to flush writes (throttled)
	FlushThrottle time.Duration `mapstructure:"flush_throttle"`

	// Interval to send pings
	PingInterval time.Duration `mapstructure:"ping_interval"`

	// Maximum wait time for pongs
	PongTimeout time.Duration `mapstructure:"pong_timeout"`

	// Process/Transport Start time
	StartTime time.Time `mapstructure:",omitempty"`
}

// DefaultMConnConfig returns the default config.
func DefaultMConnConfig() MConnConfig {
	return MConnConfig{
		SendRate:                defaultSendRate,
		RecvRate:                defaultRecvRate,
		MaxPacketMsgPayloadSize: defaultMaxPacketMsgPayloadSize,
		FlushThrottle:           defaultFlushThrottle,
		PingInterval:            defaultPingInterval,
		PongTimeout:             defaultPongTimeout,
		StartTime:               time.Now(),
	}
}

// NewMConnection wraps net.Conn and creates multiplex connection with a config
func NewMConnection(
	logger log.Logger,
	conn net.Conn,
	chDescs []*ChannelDescriptor,
	onReceive receiveCbFunc,
	config MConnConfig,
) *MConnection {
	mconn := &MConnection{
		logger:        logger,
		conn:          conn,
		bufConnReader: bufio.NewReaderSize(conn, minReadBufferSize),
		pong:          make(chan struct{}, 1),
		onReceive:     onReceive, // TODO: this should be a channel.
		config:        config,
	}
	for _, desc := range chDescs {
		mconn.channels[desc.ID] = newChannel(mconn, *desc)
	}
	return mconn
}

func (c *MConnection) Run(ctx context.Context) error {
	return scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		s.SpawnNamed("sendRoutine", func() error { return c.sendRoutine(ctx) })
		s.SpawnNamed("recvRoutine", func() error { return c.recvRoutine(ctx) })
		s.SpawnNamed("statsRoutine", func() error { return c.statsRoutine(ctx) })
		<-ctx.Done()
		// Unfortunately golang std IO operations do not support cancellation via context.
		// Instead, we trigger cancellation by closing the underlying connection.
		// Alternatively, we could utilise net.Conn.Set[Read|Write]Deadline() methods
		// for precise cancellation, but we don't have a need for that here.
		return c.conn.Close()
	})
}

func (c *MConnection) String() string {
	return fmt.Sprintf("MConn{%v}", c.conn.RemoteAddr())
}

// Queues a message to be sent to channel.
func (c *MConnection) Send(ctx context.Context, chID ChannelID, msgBytes []byte) error {
	c.logger.Debug("Send", "channel", chID, "conn", c, "msgBytes", msgBytes)

	// Send message to channel.
	channel, ok := c.channels[chID]
	if !ok {
		return fmt.Errorf("Cannot send bytes, unknown channel %X", chID)
	}
	if err:=utils.Send(ctx,channel.sendQueue,msgBytes); err!=nil {
		return fmt.Errorf("Send failed: channel = %v, conn = %v, msgBytes = %v", chID, c, msgBytes)
	}
	return nil
}

func (c *MConnection) pingRoutine(ctx context.Context) error {
	return nil
}

func (c *MConnection) statsRoutine(ctx context.Context) error {
	for {
		if err:=utils.Sleep(ctx,updateStats); err!=nil {
			return err
		}
		for _, ch := range c.channels {
			// Exponential decay of stats.
			// TODO: This is not atomic at all.
			ch.recentlySent.Store(uint64(float64(ch.recentlySent.Load())*0.8))
		}
	}
}

func (c *MConnection) popSendQueue(ctx context.Context) (*tmp2p.Packet,error) {
	for q,ctrl := range c.sendQueue.Lock() {
		for {
			if q.ping {
				q.ping = false
				q.flush = true
				return &tmp2p.Packet{
					Sum: &tmp2p.Packet_PacketPing{
						PacketPing: &tmp2p.PacketPing{},
					},
				},nil
			}
			if q.pong {
				q.pong = false
				q.flush = true
				return &tmp2p.Packet{
					Sum: &tmp2p.Packet_PacketPong{
						PacketPong: &tmp2p.PacketPong{},
					},
				},nil
			}
			// Select message.
			if q.flush { // flush every c.config.FlushThrottle if sent after flush - ping/pong flushes immediately
				return nil,nil
			}
			if err:=ctrl.Wait(ctx); err!=nil {
				return nil,err
			}
		}
	}
}

// sendRoutine polls for packets to send from channels.
func (c *MConnection) sendRoutine(ctx context.Context) (err error) {
	// We should NOT use the same rate limit for send and recv:
	// recv should be more permissive to compensate for fluctuations.
	sendMonitor := flowrate.New(c.config.StartTime, 0, 0) // sample rate: 100ms, window size: 1s
	// This doesn't make sense - TCP package is 1.5kB anyway (unless we match the encryption frame here)
	// In fact, buffering should be just moved to the encryption layer.
	bufWriter := bufio.NewWriterSize(c.conn, minWriteBufferSize)
	protoWriter := protoio.NewDelimitedWriter(bufWriter)
	for {
		msg,err := c.popSendQueue(ctx)
		if err != nil { return fmt.Errorf("popSendQueue(): %w", err) }
		if msg != nil {
			n,err:=protoWriter.Write(msg)
			if err!=nil { return fmt.Errorf("protoWriter.Write(): %w",err) }
			sendMonitor.Update(n)
		} else {
			c.logger.Debug("Flush", "conn", c)
			if err:=bufWriter.Flush(); err!=nil { return fmt.Errorf("bufWriter.Flush(): %w",err) }
		}
	}
	/*
	case // ping every c.config.PingInterval
	case // observe timeout after c.config.PongTimeout

	if time.Since(*c.lastMsgRecv.Load()) > c.config.PongTimeout {
		return errors.New("pong timeout")
	}*/
}

// Returns true if messages from channels were exhausted.
// Blocks in accordance to .sendMonitor throttling.
func (c *MConnection) sendSomePacketMsgs(ctx context.Context) error {
	// TODO: this looks bullshit: we should rate AFTER we select the message to send.
	// ALSO this is wrong layer again. We should rate limit at the level of raw bytes.
	// Block until .sendMonitor says we can write.
	// Once we're ready we send more than we asked for,
	// but amortized it should even out.
	c.sendMonitor.Limit(c._maxPacketMsgSize, c.config.SendRate, true)

	// Now send some PacketMsgs.
	for range numBatchPacketMsgs {
		if err:=c.sendPacketMsg(ctx); err!=nil {
			return err
		}
	}
	return nil
}

// Returns true if messages from channels were exhausted.
func (c *MConnection) sendPacketMsg(ctx context.Context) error {
	// Choose a channel to create a PacketMsg from.
	// The chosen channel will be the one whose recentlySent/priority is the least.
	var leastRatio float32 = math.MaxFloat32
	var leastChannel *channel
	for _, channel := range c.channels {
		// If nothing to send, skip this channel
		if !channel.isSendPending() {
			continue
		}
		// Get ratio, and keep track of lowest ratio.
		ratio := float32(channel.recentlySent.Load()) / float32(channel.desc.Priority)
		if ratio < leastRatio {
			leastRatio = ratio
			leastChannel = channel
		}
	}

	// Nothing to send?
	if leastChannel == nil {
		return nil
	}

	// Make & send a PacketMsg from this channel
	n, err := protoWriter.WriteMsg(wrapMsg(leastChannel.nextPacketMsg()))
	if err != nil {
		return fmt.Errorf("Failed to write PacketMsg: %w",err)
	}
	leastChannel.recentlySent.Add(uint64(n))
	c.sendMonitor.Update(n)
	c.flushTimer.Set()
	return nil
}

// recvRoutine reads PacketMsgs and reconstructs the message using the channels' "recving" buffer.
// After a whole message has been assembled, it's pushed to onReceive().
// Blocks depending on how the connection is throttled.
// Otherwise, it never blocks.
func (c *MConnection) recvRoutine(ctx context.Context) (err error) {
	recvMonitor := flowrate.New(config.StartTime, 0, 0)
	maxPacketMsgSize = c.maxPacketMsgSize()
	protoReader := protoio.NewDelimitedReader(c.bufConnReader, maxPacketMsgSize)

	for {
		// Block until .recvMonitor says we can read.
		recvMonitor.Limit(maxPacketMsgSize, c.config.RecvRate, true)
		packet := &tmp2p.Packet{}
		n, err := protoReader.ReadMsg(packet)
		recvMonitor.Update(n)
		if err != nil {
			return fmt.Errorf("protoReader.ReadMsg(): %w", err)
		}
		c.lastMsgRecv.Store(utils.Alloc(time.Now()))

		// Read more depending on packet type.
		switch pkt := packet.Sum.(type) {
		case *tmp2p.Packet_PacketPing:
			c.sendQueue.pong = true
		case *tmp2p.Packet_PacketPong:
			// TODO: report pong received
		case *tmp2p.Packet_PacketMsg:
			channelID := ChannelID(pkt.PacketMsg.ChannelID)
			channel, ok := c.channels[channelID]
			if pkt.PacketMsg.ChannelID < 0 || pkt.PacketMsg.ChannelID > math.MaxUint8 || !ok {
				return fmt.Errorf("unknown channel %X", pkt.PacketMsg.ChannelID)
			}
			msgBytes, err := channel.recvPacketMsg(*pkt.PacketMsg)
			if err != nil {
				return fmt.Errorf("recvPacketMsg(): %v",err)
			}
			if msgBytes != nil {
				c.logger.Debug("Received bytes", "chID", channelID, "msgBytes", msgBytes)
				// NOTE: This means the reactor.Receive runs in the same thread as the p2p recv routine
				c.onReceive(ctx, channelID, msgBytes)
			}
		default:
			return fmt.Errorf("unknown message type %v", reflect.TypeOf(packet))
		}
	}
}

// maxPacketMsgSize returns a maximum size of PacketMsg
func (c *MConnection) maxPacketMsgSize() int {
	bz, err := proto.Marshal(wrapMsg(&tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      make([]byte, c.config.MaxPacketMsgPayloadSize),
	}))
	if err != nil {
		panic(err)
	}
	return len(bz)
}

// -----------------------------------------------------------------------------
// ChannelID is an arbitrary channel ID.
type ChannelID uint8

type ChannelDescriptor struct {
	ID       ChannelID
	Priority int

	MessageType proto.Message

	// TODO: Remove once p2p refactor is complete.
	SendQueueCapacity   int
	RecvMessageCapacity int

	// RecvBufferCapacity defines the max buffer size of inbound messages for a
	// given p2p Channel queue.
	RecvBufferCapacity int

	// Human readable name of the channel, used in logging and
	// diagnostics.
	Name string
}

func (chDesc ChannelDescriptor) FillDefaults() (filled ChannelDescriptor) {
	if chDesc.SendQueueCapacity == 0 {
		chDesc.SendQueueCapacity = defaultSendQueueCapacity
	}
	if chDesc.RecvBufferCapacity == 0 {
		chDesc.RecvBufferCapacity = defaultRecvBufferCapacity
	}
	if chDesc.RecvMessageCapacity == 0 {
		chDesc.RecvMessageCapacity = defaultRecvMessageCapacity
	}
	filled = chDesc
	return
}

type sendChannel struct {
	// Exponential moving average.
	recentlySent atomic.Uint64
	desc          ChannelDescriptor
	sendQueue     [][]byte // RingBuf
}

type recvChannel struct {
	desc          ChannelDescriptor
	recving       []byte
}

func newChannel(conn *MConnection, desc ChannelDescriptor) *channel {
	desc = desc.FillDefaults()
	if desc.Priority <= 0 {
		panic("Channel default priority must be a positive integer")
	}
	return &channel{
		conn:                    conn,
		desc:                    desc,
		sendQueue:               make(chan []byte, desc.SendQueueCapacity),
		recving:                 make([]byte, 0, desc.RecvBufferCapacity),
	}
}

// Returns true if any PacketMsgs are pending to be sent.
// Call before calling nextPacketMsg()
// Goroutine-safe
func (ch *channel) isSendPending() bool {
	if len(ch.sending) == 0 {
		if len(ch.sendQueue) == 0 {
			return false
		}
		ch.sending = <-ch.sendQueue
	}
	return true
}

// Creates a new PacketMsg to send.
// Not goroutine-safe
func (ch *channel) nextPacketMsg() *tmp2p.PacketMsg {
	packet := &tmp2p.PacketMsg{ChannelID: int32(ch.desc.ID)}
	maxSize := ch.conn.config.MaxPacketMsgPayloadSize
	if len(ch.sending) <= maxSize {
		packet.EOF = true
		packet.Data = ch.sending
		ch.sending = nil
	} else {
		packet.EOF = false
		packet.Data = ch.sending[:maxSize]
		ch.sending = ch.sending[maxSize:]
	}
	return packet
}

// Handles incoming PacketMsgs. It returns a message bytes if message is
// complete, which is owned by the caller and will not be modified.
// Not goroutine-safe
func (ch *channel) recvPacketMsg(packet tmp2p.PacketMsg) ([]byte, error) {
	ch.conn.logger.Debug("Read PacketMsg", "conn", ch.conn, "packet", packet)
	if got,wantMax := len(ch.recving) + len(packet.Data), ch.desc.RecvMessageCapacity; got > wantMax {
		return nil, fmt.Errorf("received message exceeds available capacity: %v < %v", wantMax, got)
	}
	ch.recving = append(ch.recving, packet.Data...)
	if packet.EOF {
		msgBytes := ch.recving
		ch.recving = make([]byte, 0, ch.desc.RecvBufferCapacity)
		return msgBytes, nil
	}
	return nil, nil
}

func wrapMsg(m *tmp2p.PacketMsg) *tmp2p.Packet {
	return &tmp2p.Packet{
		Sum: &tmp2p.Packet_PacketMsg{
			PacketMsg: m,
		},
	}
}
