package blocksync

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/internal/consensus"
	"github.com/tendermint/tendermint/internal/eventbus"
	"github.com/tendermint/tendermint/internal/p2p"
	sm "github.com/tendermint/tendermint/internal/state"
	"github.com/tendermint/tendermint/internal/store"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/libs/utils"
	"github.com/tendermint/tendermint/libs/utils/scope"
	bcproto "github.com/tendermint/tendermint/proto/tendermint/blocksync"
	"github.com/tendermint/tendermint/types"
)

var _ service.Service = (*Reactor)(nil)

const (
	// BlockSyncChannel is a channel for blocks and status updates
	BlockSyncChannel = p2p.ChannelID(0x40)

	trySyncIntervalMS = 10

	// ask for best height every 10s
	statusUpdateIntervalSeconds = 10

	// check if we should switch to consensus reactor
	switchToConsensusIntervalSeconds = 1

	// switch to consensus after this duration of inactivity
	syncTimeout = 180 * time.Second
)

func GetChannelDescriptor() *p2p.ChannelDescriptor {
	return &p2p.ChannelDescriptor{
		ID:                  BlockSyncChannel,
		MessageType:         new(bcproto.Message),
		Priority:            5,
		SendQueueCapacity:   1000,
		RecvBufferCapacity:  1024,
		RecvMessageCapacity: MaxMsgSize,
		Name:                "blockSync",
	}
}

type consensusReactor interface {
	// For when we switch from block sync reactor to the consensus
	// machine.
	SwitchToConsensus(ctx context.Context)
}

type peerError struct {
	err    error
	peerID types.NodeID
}

func (e peerError) Error() string {
	return fmt.Sprintf("error with peer %v: %s", e.peerID, e.err.Error())
}

// Reactor handles long-term catchup syncing.
type Reactor struct {
	service.BaseService
	logger log.Logger
	start chan struct{}
	cancel context.CancelFunc

	// immutable
	initialState sm.State
	// store
	stateStore sm.Store

	blockExec             *sm.BlockExecutor
	store                 sm.BlockStore
	pool                  *BlockPool
	consReactor           consensusReactor
	previousMaxPeerHeight int64

	peerEvents  p2p.PeerEventSubscriber
	peerManager *p2p.PeerManager
	channel     *p2p.Channel

	requestsCh <-chan BlockRequest
	errorsCh   <-chan peerError

	metrics  *consensus.Metrics
	eventBus *eventbus.EventBus

	syncStartTime time.Time

	restartCh                 chan struct{}
	lastRestartTime           time.Time
	blocksBehindThreshold     uint64
	blocksBehindCheckInterval time.Duration
	restartCooldownSeconds    uint64
}

// NewReactor returns new reactor instance.
func NewReactor(
	logger log.Logger,
	stateStore sm.Store,
	blockExec *sm.BlockExecutor,
	store *store.BlockStore,
	consReactor consensusReactor,
	peerEvents p2p.PeerEventSubscriber,
	peerManager *p2p.PeerManager,
	blockSync bool,
	metrics *consensus.Metrics,
	eventBus *eventbus.EventBus,
	restartCh chan struct{},
	selfRemediationConfig *config.SelfRemediationConfig,
) *Reactor {
	r := &Reactor{
		start: make(chan struct{}),
		logger:                    logger,
		stateStore:                stateStore,
		blockExec:                 blockExec,
		store:                     store,
		consReactor:               consReactor,
		peerEvents:                peerEvents,
		peerManager:               peerManager,
		metrics:                   metrics,
		eventBus:                  eventBus,
		restartCh:                 restartCh,
		lastRestartTime:           time.Now(),
		blocksBehindThreshold:     selfRemediationConfig.BlocksBehindThreshold,
		blocksBehindCheckInterval: time.Duration(selfRemediationConfig.BlocksBehindCheckIntervalSeconds) * time.Second,
		restartCooldownSeconds:    selfRemediationConfig.RestartCooldownSeconds,
	}
	// If blockSync, then reactor should start immediately.
	if blockSync {
		close(r.start)
	}
	r.BaseService = *service.NewBaseService(logger, "BlockSync", r)
	return r
}

func (r *Reactor) SetChannel(ch *p2p.Channel) {
	r.channel = ch
}

// OnStart starts separate go routines for each p2p Channel and listens for
// envelopes on each. In addition, it also listens for peer updates and handles
// messages on that p2p channel accordingly. The caller must be sure to execute
// OnStop to ensure the outbound p2p Channels are closed.
//
// If blockSync is enabled, we also start the pool and the pool processing
// goroutine. If the pool fails to start, an error is returned.
func (r *Reactor) OnStart(ctx context.Context) error {
	// This context is bound to the reactor lifetime,
	// which is unrelated to the ctx passed to OnStart.
	ctx,cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		defer cancel()
		err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
			// Reactor starts only after receiving an async start signal.
			if _,_,err := utils.RecvOrClosed(ctx, r.start); err!=nil {
				return err
			}
			state, err := r.stateStore.Load()
			if err != nil {
				return err
			}
			r.initialState = state
			r.lastRestartTime = time.Now()

			if state.LastBlockHeight != r.store.Height() {
				return fmt.Errorf("state (%v) and store (%v) height mismatch", state.LastBlockHeight, r.store.Height())
			}

			startHeight := r.store.Height() + 1
			if startHeight == 1 {
				startHeight = state.InitialHeight
			}

			requestsCh := make(chan BlockRequest, maxTotalRequesters)
			errorsCh := make(chan peerError, maxPeerErrBuffer) // NOTE: The capacity should be larger than the peer count.
			r.pool = NewBlockPool(r.logger, startHeight, requestsCh, errorsCh, r.peerManager)
			r.requestsCh = requestsCh
			r.errorsCh = errorsCh

			if err := r.pool.Start(ctx); err != nil {
				return err
			}
			s.Spawn(func() error { return r.requestRoutine(ctx) })
			s.Spawn(func() error { return r.poolRoutine(ctx) })
			s.Spawn(func() error { return r.processBlockSyncCh(ctx) })
			s.Spawn(func() error { return r.processPeerUpdates(ctx, r.peerEvents(ctx)) })
			if r.consReactor != nil {
				r.consReactor.SwitchToConsensus(ctx)
			}
			return nil
		})
		if utils.IgnoreCancel(err)!=nil {
			r.logger.Error("blocksync.Reactor", "err", err)
		}
	}()
	return nil
}

// OnStop stops the reactor by signaling to all spawned goroutines to exit and
// blocking until they all exit.
func (r *Reactor) OnStop() {
	if r.cancel != nil {
		r.cancel()
	}
}

// respondToPeer loads a block and sends it to the requesting peer, if we have it.
// Otherwise, we'll respond saying we do not have it.
func (r *Reactor) respondToPeer(ctx context.Context, msg *bcproto.BlockRequest, peerID types.NodeID) error {
	block := r.store.LoadBlock(msg.Height)
	if block == nil {
		r.logger.Info("peer requesting a block we do not have", "peer", peerID, "height", msg.Height)
		return r.channel.Send(ctx, p2p.Envelope{
			To:      peerID,
			Message: &bcproto.NoBlockResponse{Height: msg.Height},
		})
	}

	state, err := r.stateStore.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	var extCommit *types.ExtendedCommit
	if state.ConsensusParams.ABCI.VoteExtensionsEnabled(msg.Height) {
		extCommit = r.store.LoadBlockExtendedCommit(msg.Height)
		if extCommit == nil {
			return fmt.Errorf("found block in store with no extended commit: %v", block)
		}
	}

	blockProto, err := block.ToProto()
	if err != nil {
		return fmt.Errorf("failed to convert block to protobuf: %w", err)
	}

	return r.channel.Send(ctx, p2p.Envelope{
		To: peerID,
		Message: &bcproto.BlockResponse{
			Block:     blockProto,
			ExtCommit: extCommit.ToProto(),
		},
	})
}

// handleMessage handles an Envelope sent from a peer on a specific p2p Channel.
// It will handle errors and any possible panics gracefully. A caller can handle
// any error returned by sending a PeerError on the respective channel.
func (r *Reactor) handleMessage(ctx context.Context, envelope *p2p.Envelope) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("panic in processing message: %v", e)
			r.logger.Error(
				"recovering from processing message panic",
				"err", err,
				"stack", string(debug.Stack()),
			)
		}
	}()

	r.logger.Debug("received message", "message", envelope.Message, "peer", envelope.From)

	switch envelope.ChannelID {
	case BlockSyncChannel:
		switch msg := envelope.Message.(type) {
		case *bcproto.BlockRequest:
			return r.respondToPeer(ctx, msg, envelope.From)
		case *bcproto.BlockResponse:
			block, err := types.BlockFromProto(msg.Block)

			r.logger.Info("received block response from peer",
				"peer", envelope.From,
				"height", block.Height)
			if err != nil {
				r.logger.Error("failed to convert block from proto",
					"peer", envelope.From,
					"err", err)
				return err
			}
			var extCommit *types.ExtendedCommit
			if msg.ExtCommit != nil {
				var err error
				extCommit, err = types.ExtendedCommitFromProto(msg.ExtCommit)
				if err != nil {
					r.logger.Error("failed to convert extended commit from proto",
						"peer", envelope.From,
						"err", err)
					return err
				}
			}

			if err := r.pool.AddBlock(envelope.From, block, extCommit, block.Size()); err != nil {
				r.logger.Error("failed to add block", "err", err)
			}

		case *bcproto.StatusRequest:
			return r.channel.Send(ctx, p2p.Envelope{
				To: envelope.From,
				Message: &bcproto.StatusResponse{
					Height: r.store.Height(),
					Base:   r.store.Base(),
				},
			})
		case *bcproto.StatusResponse:
			r.pool.SetPeerRange(envelope.From, msg.Base, msg.Height)

		case *bcproto.NoBlockResponse:
			r.logger.Debug("peer does not have the requested block",
				"peer", envelope.From,
				"height", msg.Height)

		default:
			return fmt.Errorf("received unknown message: %T", msg)
		}

	default:
		err = fmt.Errorf("unknown channel ID (%d) for envelope (%v)", envelope.ChannelID, envelope)
	}

	return err
}

// processBlockSyncCh initiates a blocking process where we listen for and handle
// envelopes on the BlockSyncChannel and blockSyncOutBridgeCh. Any error encountered during
// message execution will result in a PeerError being sent on the BlockSyncChannel.
// When the reactor is stopped, we will catch the signal and close the p2p Channel
// gracefully.
func (r *Reactor) processBlockSyncCh(ctx context.Context) error {
	iter := r.channel.Receive(ctx)
	for iter.Next(ctx) {
		envelope := iter.Envelope()
		if err := r.handleMessage(ctx, envelope); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			r.logger.Error("failed to process message", "ch_id", envelope.ChannelID, "envelope", envelope, "err", err)
			if serr := r.channel.SendError(ctx, p2p.PeerError{
				NodeID: envelope.From,
				Err:    err,
			}); serr != nil {
				return serr
			}
		}
	}
	return nil
}

// processPeerUpdate processes a PeerUpdate.
func (r *Reactor) processPeerUpdate(ctx context.Context, peerUpdate p2p.PeerUpdate) error {
	r.logger.Debug("received peer update", "peer", peerUpdate.NodeID, "status", peerUpdate.Status)

	// XXX: Pool#RedoRequest can sometimes give us an empty peer.
	if len(peerUpdate.NodeID) == 0 {
		return nil
	}

	switch peerUpdate.Status {
	case p2p.PeerStatusUp:
		// send a status update the newly added peer
		if err := r.channel.Send(ctx, p2p.Envelope{
			To: peerUpdate.NodeID,
			Message: &bcproto.StatusResponse{
				Base:   r.store.Base(),
				Height: r.store.Height(),
			},
		}); err != nil {
			r.pool.RemovePeer(peerUpdate.NodeID)
			if err := r.channel.SendError(ctx, p2p.PeerError{
				NodeID: peerUpdate.NodeID,
				Err:    err,
			}); err != nil {
				return err
			}
		}
	case p2p.PeerStatusDown:
		r.pool.RemovePeer(peerUpdate.NodeID)
	}
	return nil
}

// processPeerUpdates initiates a blocking process where we listen for and handle
// PeerUpdate messages. When the reactor is stopped, we will catch the signal and
// close the p2p PeerUpdatesCh gracefully.
func (r *Reactor) processPeerUpdates(ctx context.Context, peerUpdates *p2p.PeerUpdates) error {
	for {
		peerUpdate, err := utils.Recv(ctx,peerUpdates.Updates())
		if err != nil {
			return nil
		}
		if err := r.processPeerUpdate(ctx, peerUpdate); err!=nil {
			r.logger.Info("r.processPeerUpdate()","err",err)
		}
	}
}

// SwitchToBlockSync is called by the state sync reactor when switching to fast
// sync.
func (r *Reactor) SwitchToBlockSync(ctx context.Context, state sm.State) error {
	r.initialState = state
	r.pool.height = state.LastBlockHeight + 1
	r.syncStartTime = time.Now()
	close(r.start) // signal the reactor to start

	if err := r.PublishStatus(types.EventDataBlockSyncStatus{
		Complete: false,
		Height:   state.LastBlockHeight,
	}); err != nil {
		return err
	}

	return nil
}

func (r *Reactor) requestRoutine(ctx context.Context) error {
	statusUpdateTicker := time.NewTicker(statusUpdateIntervalSeconds * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil
		case request := <-r.requestsCh:
			if err := r.channel.Send(ctx, p2p.Envelope{
				To:      request.PeerID,
				Message: &bcproto.BlockRequest{Height: request.Height},
			}); err != nil {
				if err := r.channel.SendError(ctx, p2p.PeerError{
					NodeID: request.PeerID,
					Err:    err,
				}); err != nil {
					return err
				}
			}
		case pErr := <-r.errorsCh:
			if err := r.channel.SendError(ctx, p2p.PeerError{
				NodeID: pErr.peerID,
				Err:    pErr.err,
			}); err != nil {
				return err
			}
		case <-statusUpdateTicker.C:
			if err := r.channel.Send(ctx, p2p.Envelope{
				Broadcast: true,
				Message:   &bcproto.StatusRequest{},
			}); err != nil {
				return err
			}
		}
	}
}

// poolRoutine handles messages from the poolReactor telling the reactor what to
// do.
//
// NOTE: Don't sleep in the FOR_LOOP or otherwise slow it down!
func (r *Reactor) poolRoutine(ctx context.Context) error {
	var (
		trySyncTicker           = time.NewTicker(trySyncIntervalMS * time.Millisecond)
		switchToConsensusTicker = time.NewTicker(switchToConsensusIntervalSeconds * time.Second)
		lastApplyBlockTime      = time.Now()

		blocksSynced = uint64(0)

		chainID = r.initialState.ChainID
		state   = r.initialState

		lastHundred = time.Now()
		lastRate    = 0.0

		didProcessCh = make(chan struct{}, 1)

		initialCommitHasExtensions = (r.initialState.LastBlockHeight > 0 && r.store.LoadBlockExtendedCommit(r.initialState.LastBlockHeight) != nil)
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-switchToConsensusTicker.C:
			var (
				height, numPending, lenRequesters = r.pool.GetStatus()
				lastAdvance                       = r.pool.LastAdvance()
			)

			r.logger.Debug(
				"consensus ticker",
				"num_pending", numPending,
				"total", lenRequesters,
				"height", height,
			)

			switch {

			// The case statement below is a bit confusing, so here is a breakdown
			// of its logic and purpose:
			//
			// If VoteExtensions are enabled we cannot switch to consensus without
			// the vote extension data for the previous height, i.e. state.LastBlockHeight.
			//
			// If extensions were required during state.LastBlockHeight and we have
			// sync'd at least one block, then we are guaranteed to have extensions.
			// BlockSync requires that the blocks it fetches have extensions if
			// extensions were enabled during the height.
			//
			// If extensions were required during state.LastBlockHeight and we have
			// not sync'd any blocks, then we can only transition to Consensus
			// if we already had extensions for the initial height.
			// If any of these conditions is not met, we continue the loop, looking
			// for extensions.
			case state.ConsensusParams.ABCI.VoteExtensionsEnabled(state.LastBlockHeight) &&
				(blocksSynced == 0 && !initialCommitHasExtensions):
				r.logger.Info(
					"no extended commit yet",
					"height", height,
					"last_block_height", state.LastBlockHeight,
					"initial_height", state.InitialHeight,
					"max_peer_height", r.pool.MaxPeerHeight(),
					"timeout_in", syncTimeout-time.Since(lastAdvance),
				)
				continue

			case r.pool.IsCaughtUp() && r.previousMaxPeerHeight <= r.pool.MaxPeerHeight():
				r.logger.Info("switching to consensus reactor after caught up", "height", height)

			case time.Since(lastAdvance) > syncTimeout:
				r.logger.Error("no progress since last advance", "last_advance", lastAdvance)
				continue

			default:
				r.logger.Info(
					"not caught up yet",
					"height", height,
					"max_peer_height", r.pool.MaxPeerHeight(),
					"timeout_in", syncTimeout-time.Since(lastAdvance),
				)
				continue
			}
			continue

		case <-trySyncTicker.C:
			select {
			case didProcessCh <- struct{}{}:
			default:
			}
		case <-didProcessCh:
			// NOTE: It is a subtle mistake to process more than a single block at a
			// time (e.g. 10) here, because we only send one BlockRequest per loop
			// iteration. The ratio mismatch can result in starving of blocks, i.e. a
			// sudden burst of requests and responses, and repeat. Consequently, it is
			// better to split these routines rather than coupling them as it is
			// written here.
			//
			// TODO: Uncouple from request routine.

			// see if there are any blocks to sync
			first, second, extCommit := r.pool.PeekTwoBlocks()
			if first != nil && extCommit == nil &&
				state.ConsensusParams.ABCI.VoteExtensionsEnabled(first.Height) {
				// See https://github.com/tendermint/tendermint/pull/8433#discussion_r866790631
				panic(fmt.Errorf("peeked first block without extended commit at height %d - possible node store corruption", first.Height))
			} else if first == nil || second == nil {
				// we need to have fetched two consecutive blocks in order to perform blocksync verification
				continue
			}

			// try again quickly next loop
			didProcessCh <- struct{}{}

			firstParts, err := first.MakePartSet(types.BlockPartSizeBytes)
			if err != nil {
				r.logger.Error("failed to make ",
					"height", first.Height,
					"err", err.Error())
				return err
			}

			var (
				firstPartSetHeader = firstParts.Header()
				firstID            = types.BlockID{Hash: first.Hash(), PartSetHeader: firstPartSetHeader}
			)

			// Finally, verify the first block using the second's commit.
			//
			// NOTE: We can probably make this more efficient, but note that calling
			// first.Hash() doesn't verify the tx contents, so MakePartSet() is
			// currently necessary.
			// TODO(sergio): Should we also validate against the extended commit?
			err = state.Validators.VerifyCommitLight(chainID, firstID, first.Height, second.LastCommit)

			if err == nil {
				// validate the block before we persist it
				err = r.blockExec.ValidateBlock(ctx, state, first)
			}
			if err == nil && state.ConsensusParams.ABCI.VoteExtensionsEnabled(first.Height) {
				// if vote extensions were required at this height, ensure they exist.
				err = extCommit.EnsureExtensions()
			}
			// If either of the checks failed we log the error and request for a new block
			// at that height
			if err != nil {
				r.logger.Error(
					err.Error(),
					"last_commit", second.LastCommit,
					"block_id", firstID,
					"height", first.Height,
				)

				// NOTE: We've already removed the peer's request, but we still need
				// to clean up the rest.
				peerID := r.pool.RedoRequest(first.Height)
				if serr := r.channel.SendError(ctx, p2p.PeerError{
					NodeID: peerID,
					Err:    err,
				}); serr != nil {
					return serr
				}

				peerID2 := r.pool.RedoRequest(second.Height)
				if peerID2 != peerID {
					if serr := r.channel.SendError(ctx, p2p.PeerError{
						NodeID: peerID2,
						Err:    err,
					}); serr != nil {
						return serr
					}
				}
				// TODO(gprusak): shuldn't it be continue?
				return err
			}

			r.pool.PopRequest()

			// TODO: batch saves so we do not persist to disk every block
			if state.ConsensusParams.ABCI.VoteExtensionsEnabled(first.Height) {
				r.store.SaveBlockWithExtendedCommit(first, firstParts, extCommit)
			} else {
				// We use LastCommit here instead of extCommit. extCommit is not
				// guaranteed to be populated by the peer if extensions are not enabled.
				// Currently, the peer should provide an extCommit even if the vote extension data are absent
				// but this may change so using second.LastCommit is safer.
				r.store.SaveBlock(first, firstParts, second.LastCommit)
			}

			// TODO: Same thing for app - but we would need a way to get the hash
			// without persisting the state.
			r.logger.Info(fmt.Sprintf("Requesting block %d from peer took %s", first.Height, time.Since(lastApplyBlockTime)))
			startTime := time.Now()
			state, err = r.blockExec.ApplyBlock(ctx, state, firstID, first, nil)
			r.logger.Info(fmt.Sprintf("ApplyBlock %d took %s", first.Height, time.Since(startTime)))
			lastApplyBlockTime = time.Now()
			if err != nil {
				panic(fmt.Sprintf("failed to process committed block (%d:%X): %v", first.Height, first.Hash(), err))
			}

			r.metrics.RecordConsMetrics(first)

			blocksSynced++

			if blocksSynced%100 == 0 {
				lastRate = 0.9*lastRate + 0.1*(100/time.Since(lastHundred).Seconds())
				r.logger.Info(
					"block sync rate",
					"height", r.pool.height,
					"max_peer_height", r.pool.MaxPeerHeight(),
					"blocks/s", lastRate,
				)

				lastHundred = time.Now()
			}
		}
	}
}

func (r *Reactor) GetMaxPeerBlockHeight() int64 {
	return r.pool.MaxPeerHeight()
}

func (r *Reactor) PublishStatus(event types.EventDataBlockSyncStatus) error {
	if r.eventBus == nil {
		return errors.New("event bus is not configured")
	}
	return r.eventBus.PublishEventBlockSyncStatus(event)
}
