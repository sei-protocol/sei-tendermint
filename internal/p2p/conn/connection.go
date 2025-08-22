package conn

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"net"
	"reflect"
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
	// TODO(gprusak): these are enormous values. Consider decreasing to ~10s.
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
*/

type MConnection struct {
	logger log.Logger
	config        MConnConfig
	conn          net.Conn
	bufConnReader *bufio.Reader
	sendQueue     utils.Watch[*sendQueue]
	flushTime     utils.AtomicWatch[utils.Option[time.Time]]

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
type sendQueue struct {
	ping bool
	pong bool
	flush utils.Option[time.Time]
	channels map[ChannelID]*sendChannel
}

func newSendQueue(chDescs []*ChannelDescriptor) *sendQueue {
	q := &sendQueue{
		channels: map[ChannelID]*sendChannel{},
	}
	for _, desc := range chDescs {
		desc := desc.withDefaults()
		q.channels[desc.ID] = &sendChannel{
			id: desc.ID,
			priority: desc.Priority,
			queue: utils.NewRingBuf[*[]byte](int(desc.SendQueueCapacity)),
		}
	}
	return q
}

func (q *sendQueue) setFlush(t time.Time) {
	if old,ok := q.flush.Get(); ok && old.Before(t) {
		return
	}
	q.flush = utils.Some(t)
}

// NewMConnection wraps net.Conn and creates multiplex connection with a config
func NewMConnection(
	logger log.Logger,
	conn net.Conn,
	chDescs []*ChannelDescriptor,
	onReceive receiveCbFunc,
	config MConnConfig,
) *MConnection {
	return &MConnection{
		logger:        logger,
		config:        config,
		conn:          conn,
		sendQueue:     utils.NewWatch(newSendQueue(chDescs)),
		onReceive:     onReceive, // TODO: this should be a channel.
	}
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

// Queues a message to be sent.
// WARNING: takes ownership of msgBytes
// TODO(gprusak): fix the ownership
func (c *MConnection) Send(ctx context.Context, chID ChannelID, msgBytes []byte) error {
	c.logger.Debug("Send", "channel", chID, "conn", c, "msgBytes", msgBytes)
	for q,ctrl := range c.sendQueue.Lock() {
		ch,ok := q.channels[chID]
		if !ok {
			return fmt.Errorf("unknown channel %X", chID)
		}
		if err:=ctrl.WaitUntil(ctx, func() bool { return !ch.queue.Full() }); err!=nil {
			return err
		}
		ch.queue.PushBack(&msgBytes)
		ctrl.Updated()
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
		for q := range c.sendQueue.Lock() {
			for _, ch := range q.channels {
				// Exponential decay of stats.
				// TODO(gprusak): This is not atomic at all.
				ch.recentlySent.Store(uint64(float64(ch.recentlySent.Load())*0.8))
			}
		}
	}
}

// popSendQueue pops a message from the send queue.
// Returns nil,nil if the connection should be flushed.
func (c *MConnection) popSendQueue(ctx context.Context) (*tmp2p.Packet,error) {
	for q,ctrl := range c.sendQueue.Lock() {
		for {
			if q.ping {
				q.ping = false
				q.setFlush(time.Now())
				return &tmp2p.Packet{
					Sum: &tmp2p.Packet_PacketPing{
						PacketPing: &tmp2p.PacketPing{},
					},
				},nil
			}
			if q.pong {
				q.pong = false
				q.setFlush(time.Now())
				return &tmp2p.Packet{
					Sum: &tmp2p.Packet_PacketPong{
						PacketPong: &tmp2p.PacketPong{},
					},
				},nil
			}
			// Choose a channel to create a PacketMsg from.
			// The chosen channel will be the one whose recentlySent/priority is the least.
			leastRatio := float32(math.Inf(1))
			var leastChannel *sendChannel
			for _, channel := range q.channels {
				if channel.queue.Len()==0 { continue }
				if ratio := channel.ratio(); ratio < leastRatio {
					leastRatio = ratio
					leastChannel = channel
				}
			}
			if leastChannel != nil {
				q.setFlush(time.Now().Add(c.config.FlushThrottle))
				msg := leastChannel.popMsg(c.config.MaxPacketMsgPayloadSize)
				leastChannel.recentlySent.Add(uint64(len(msg.Data)))
				return &tmp2p.Packet{
					Sum: &tmp2p.Packet_PacketMsg{
						PacketMsg: msg,
					},
				}, nil
			}
			if err := utils.WithDeadline(ctx, q.flush, func(ctx context.Context) error {
				return ctrl.Wait(ctx)
			}); err!=nil {
				if ctx.Err() == nil {
					// It is flush time!
					q.flush = utils.None[time.Time]()
					return nil, nil
				}
				return nil,err
			}
		}
	}
	panic("unreachable")
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
			n,err:=protoWriter.WriteMsg(msg)
			if err!=nil { return fmt.Errorf("protoWriter.Write(): %w",err) }
			sendMonitor.Update(n)
		} else {
			c.logger.Debug("Flush", "conn", c)
			if err:=bufWriter.Flush(); err!=nil { return fmt.Errorf("bufWriter.Flush(): %w",err) }
		}
	}
}

// recvRoutine reads PacketMsgs and reconstructs the message using the channels' "recving" buffer.
// After a whole message has been assembled, it's pushed to onReceive().
// Blocks depending on how the connection is throttled.
// Otherwise, it never blocks.
func (c *MConnection) recvRoutine(ctx context.Context) (err error) {
	/*
	case // ping every c.config.PingInterval
	case // observe timeout after c.config.PongTimeout

	if time.Since(*c.lastMsgRecv.Load()) > c.config.PongTimeout {
		return errors.New("pong timeout")
	}*/
	recvMonitor := flowrate.New(c.config.StartTime, 0, 0)
	maxPacketMsgSize := c.maxPacketMsgSize()
	bufReader := bufio.NewReaderSize(c.conn, minReadBufferSize)
	protoReader := protoio.NewDelimitedReader(bufReader, maxPacketMsgSize)
	channels := map[ChannelID]*recvChannel{}
	for q := range c.sendQueue.Lock() {
		for _, ch := range q.channels {
			channels[ch.desc.ID] = newRecvChannel(ch.desc)
		}
	}

	lastMsgRecv := time.Now()
	for {
		recvMonitor.Limit(maxPacketMsgSize, c.config.RecvRate, true)
		packet := &tmp2p.Packet{}
		n, err := protoReader.ReadMsg(packet)
		if err != nil {
			return fmt.Errorf("protoReader.ReadMsg(): %w", err)
		}
		recvMonitor.Update(n)
		lastMsgRecv = time.Now()

		// Read more depending on packet type.
		switch p := packet.Sum.(type) {
		case *tmp2p.Packet_PacketPing:
			for q,ctrl := range c.sendQueue.Lock() {
				q.pong = true
				ctrl.Updated()
			}
		case *tmp2p.Packet_PacketPong:
		case *tmp2p.Packet_PacketMsg:
			channelID, castOk := utils.SafeCast[ChannelID](p.PacketMsg.ChannelID)
			ch, ok := channels[channelID]
			if !castOk || !ok {
				return fmt.Errorf("unknown channel %X", p.PacketMsg.ChannelID)
			}
			c.logger.Debug("Read PacketMsg", "conn", c, "packet", packet)
			msgBytes, err := ch.pushMsg(p.PacketMsg)
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
	bz, err := proto.Marshal(&tmp2p.Packet{
		Sum: &tmp2p.Packet_PacketMsg{
			PacketMsg: &tmp2p.PacketMsg{
				ChannelID: 0x01,
				EOF:       true,
				Data:      make([]byte, c.config.MaxPacketMsgPayloadSize),
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return len(bz)
}

// ChannelID is an arbitrary channel ID.
type ChannelID uint8

type ChannelDescriptor struct {
	ID       ChannelID
	Priority uint

	MessageType proto.Message

	// TODO: Remove once p2p refactor is complete.
	SendQueueCapacity   uint
	RecvMessageCapacity uint

	// RecvBufferCapacity defines the max buffer size of inbound messages for a
	// given p2p Channel queue.
	RecvBufferCapacity uint

	// Human readable name of the channel, used in logging and
	// diagnostics.
	Name string
}

func (chDesc ChannelDescriptor) withDefaults() ChannelDescriptor {
	if chDesc.Priority == 0 {
		chDesc.Priority = 1
	}
	if chDesc.SendQueueCapacity == 0 {
		chDesc.SendQueueCapacity = defaultSendQueueCapacity
	}
	if chDesc.RecvBufferCapacity == 0 {
		chDesc.RecvBufferCapacity = defaultRecvBufferCapacity
	}
	if chDesc.RecvMessageCapacity == 0 {
		chDesc.RecvMessageCapacity = defaultRecvMessageCapacity
	}
	return chDesc
}

type sendChannel struct {
	id 					  ChannelID
	priority      uint
	recentlySent  atomic.Uint64 // Exponential moving average.
	queue         utils.RingBuf[*[]byte]
}

func (ch *sendChannel) ratio() float32 {
	return float32(ch.recentlySent.Load()) / float32(ch.priority)
}

// Creates a new PacketMsg to send.
// Not goroutine-safe
func (ch *sendChannel) popMsg(maxPayload int) *tmp2p.PacketMsg {
  payload := ch.queue.Get(0)
	packet := &tmp2p.PacketMsg{ChannelID: int32(ch.id)}
	if len(*payload) <= maxPayload {
		packet.EOF = true
		packet.Data = *ch.queue.PopFront()
	} else {
		packet.EOF = false
		packet.Data = (*payload)[:maxPayload]
		*payload = (*payload)[maxPayload:]
	}
	return packet
}

type recvChannel struct {
	desc ChannelDescriptor
	buf []byte
}

func newRecvChannel(desc ChannelDescriptor) *recvChannel {
	return &recvChannel{
		desc: desc.withDefaults(),
		buf: make([]byte, 0, desc.RecvBufferCapacity),
	}
}

// Handles incoming PacketMsgs. It returns a message bytes if message is
// complete, which is owned by the caller and will not be modified.
// Not goroutine-safe
func (ch *recvChannel) pushMsg(packet *tmp2p.PacketMsg) ([]byte, error) {
	if got,wantMax := len(ch.buf) + len(packet.Data), ch.desc.RecvMessageCapacity; uint(got) > wantMax {
		return nil, fmt.Errorf("received message exceeds available capacity: %v < %v", wantMax, got)
	}
	ch.buf = append(ch.buf, packet.Data...)
	if packet.EOF {
		msgBytes := ch.buf
		ch.buf = make([]byte, 0, ch.desc.RecvBufferCapacity)
		return msgBytes, nil
	}
	return nil, nil
}
