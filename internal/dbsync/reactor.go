package dbsync

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/internal/p2p"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/light"
	dstypes "github.com/tendermint/tendermint/proto/tendermint/dbsync"
	"github.com/tendermint/tendermint/types"
)

const (
	// MetadataChannel exchanges DB metadata
	MetadataChannel = p2p.ChannelID(0x70)

	// FileChannel exchanges file data
	FileChannel = p2p.ChannelID(0x71)

	MetadataHeightFilename     = "LATEST_HEIGHT"
	MetadataSubdirectoryPrefix = "snapshot_"
	MetadataFilename           = "METADATA"
)

type Reactor struct {
	service.BaseService
	logger log.Logger

	// Dispatcher is used to multiplex light block requests and responses over multiple
	// peers used by the p2p state provider and in reverse sync.
	dispatcher *light.Dispatcher
	peers      *light.PeerList

	chainID       string
	config        config.DBSyncConfig
	providers     map[types.NodeID]*light.BlockProvider
	stateProvider light.StateProvider

	syncer *Syncer

	mtx sync.RWMutex
}

func NewReactor(logger log.Logger, config config.DBSyncConfig) *Reactor {
	return &Reactor{
		logger: logger,
		syncer: NewSyncer(config.Enable, config.TimeoutInSeconds),
	}
}

func (r *Reactor) OnStart(ctx context.Context) error {
	return nil
}

func (r *Reactor) handleMetadataRequest(ctx context.Context, req *dstypes.MetadataRequest, from types.NodeID, metadataCh *p2p.Channel) (err error) {
	responded := false
	defer func() {
		if err != nil {
			r.logger.Debug(fmt.Sprintf("handle metadata request encountered error %s", err))
		}
		if !responded {
			err = metadataCh.Send(ctx, p2p.Envelope{
				To: from,
				Message: &dstypes.MetadataResponse{
					Height:    0,
					Hash:      []byte{},
					Filenames: []string{},
				},
			})
		}
	}()

	if r.config.SnapshotDirectory == "" {
		return
	}

	metadataHeightFile := filepath.Join(r.config.SnapshotDirectory, MetadataHeightFilename)
	heightData, err := os.ReadFile(metadataHeightFile)
	if err != nil {
		err = fmt.Errorf("cannot read height file %s due to %s", metadataHeightFile, err)
		return
	}
	height := binary.BigEndian.Uint64(heightData)
	heightSubdirectory := filepath.Join(r.config.SnapshotDirectory, fmt.Sprintf("%s%d", MetadataSubdirectoryPrefix, height))
	metadataFilename := filepath.Join(heightSubdirectory, MetadataFilename)
	data, err := os.ReadFile(metadataFilename)
	if err != nil {
		err = fmt.Errorf("cannot read metadata file %s due to %s", metadataFilename, err)
		return
	}
	msg := dstypes.MetadataResponse{}
	err = msg.Unmarshal(data)
	if err != nil {
		err = fmt.Errorf("cannot unmarshal metadata file %s due to %s", metadataFilename, err)
		return
	}
	err = metadataCh.Send(ctx, p2p.Envelope{
		To:      from,
		Message: &msg,
	})
	responded = true
	return
}

func (r *Reactor) handleMetadataMessage(ctx context.Context, envelope *p2p.Envelope, metadataCh *p2p.Channel) error {
	logger := r.logger.With("peer", envelope.From)

	switch msg := envelope.Message.(type) {
	case *dstypes.MetadataRequest:
		return r.handleMetadataRequest(ctx, msg, envelope.From, metadataCh)

	case *dstypes.MetadataResponse:
		logger.Info("received metadata", "height", msg.Height, "size", len(msg.Filenames))
		r.syncer.SetMetadata(msg)

	default:
		return fmt.Errorf("received unknown message: %T", msg)
	}

	return nil
}

func (r *Reactor) handleFileMessage(ctx context.Context, envelope *p2p.Envelope, fileCh *p2p.Channel) error {
	logger := r.logger.With("peer", envelope.From)

	switch msg := envelope.Message.(type) {
	case *dstypes.FileRequest:
		return r.handleMetadataRequest(ctx, msg, envelope.From, metadataCh)

	case *dstypes.FileResponse:
		logger.Info("received metadata", "height", msg.Height, "size", len(msg.Filenames))
		r.syncer.SetMetadata(msg)

	default:
		return fmt.Errorf("received unknown message: %T", msg)
	}

	return nil
}

func (r *Reactor) processPeerUpdate(ctx context.Context, peerUpdate p2p.PeerUpdate) {
	r.logger.Debug("received peer update", "peer", peerUpdate.NodeID, "status", peerUpdate.Status)

	switch peerUpdate.Status {
	case p2p.PeerStatusUp:
		if peerUpdate.Channels.Contains(MetadataChannel) && peerUpdate.Channels.Contains(FileChannel) {
			r.peers.Append(peerUpdate.NodeID)
		} else {
			r.logger.Error("could not use peer for dbsync (removing)", "peer", peerUpdate.NodeID)
			r.peers.Remove(peerUpdate.NodeID)
		}
	case p2p.PeerStatusDown:
		r.peers.Remove(peerUpdate.NodeID)
	}

	r.mtx.Lock()
	defer r.mtx.Unlock()

	switch peerUpdate.Status {
	case p2p.PeerStatusUp:
		newProvider := light.NewBlockProvider(peerUpdate.NodeID, r.chainID, r.dispatcher)

		r.providers[peerUpdate.NodeID] = newProvider
		if sp, ok := r.stateProvider.(*light.StateProviderP2P); ok {
			// we do this in a separate routine to not block whilst waiting for the light client to finish
			// whatever call it's currently executing
			go sp.AddProvider(newProvider)
		}

	case p2p.PeerStatusDown:
		delete(r.providers, peerUpdate.NodeID)
	}
	r.logger.Debug("processed peer update", "peer", peerUpdate.NodeID, "status", peerUpdate.Status)
}

func (r *Reactor) processPeerUpdates(ctx context.Context, peerUpdates *p2p.PeerUpdates) {
	for {
		select {
		case <-ctx.Done():
			return
		case peerUpdate := <-peerUpdates.Updates():
			r.processPeerUpdate(ctx, peerUpdate)
		}
	}
}
