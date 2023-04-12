package dbsync

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sync"
	"time"

	"github.com/tendermint/tendermint/config"
	sm "github.com/tendermint/tendermint/internal/state"
	"github.com/tendermint/tendermint/libs/log"
	dstypes "github.com/tendermint/tendermint/proto/tendermint/dbsync"
	"github.com/tendermint/tendermint/types"
)

const ApplicationDBSubdirectory = "application.db"

type Syncer struct {
	mtx    *sync.RWMutex
	logger log.Logger

	active                 bool
	heightToSync           uint64
	peersToSync            []types.NodeID
	expectedChecksums      map[string][]byte
	pendingFiles           map[string]struct{}
	syncedFiles            map[string]struct{}
	completionSignals      map[string]chan struct{}
	metadataSetAt          time.Time
	timeoutInSeconds       time.Duration
	fileQueue              []*dstypes.FileResponse
	applicationDBDirectory string
	sleepInSeconds         time.Duration
	fileWorkerCount        int
	fileWorkerTimeout      time.Duration
	fileWorkerCancelFn     context.CancelFunc

	metadataRequestFn func(context.Context) error
	fileRequestFn     func(context.Context, types.NodeID, uint64, string) error
	commitStateFn     func(context.Context, uint64) (sm.State, *types.Commit, error)
	postSyncFn        func(sm.State, *types.Commit) error

	state  sm.State
	commit *types.Commit
}

func NewSyncer(
	logger log.Logger,
	dbsyncConfig config.DBSyncConfig,
	baseConfig config.BaseConfig,
	metadataRequestFn func(context.Context) error,
	fileRequestFn func(context.Context, types.NodeID, uint64, string) error,
	commitStateFn func(context.Context, uint64) (sm.State, *types.Commit, error),
	postSyncFn func(sm.State, *types.Commit) error,
) *Syncer {
	return &Syncer{
		logger:                 logger,
		active:                 dbsyncConfig.Enable,
		timeoutInSeconds:       time.Duration(dbsyncConfig.TimeoutInSeconds),
		fileQueue:              []*dstypes.FileResponse{},
		applicationDBDirectory: path.Join(baseConfig.DBDir(), ApplicationDBSubdirectory),
		sleepInSeconds:         time.Duration(dbsyncConfig.NoFileSleepInSeconds),
		fileWorkerCount:        dbsyncConfig.FileWorkerCount,
		fileWorkerTimeout:      time.Duration(dbsyncConfig.FileWorkerTimeout),
		metadataRequestFn:      metadataRequestFn,
		fileRequestFn:          fileRequestFn,
		commitStateFn:          commitStateFn,
		postSyncFn:             postSyncFn,
		mtx:                    &sync.RWMutex{},
	}
}

func (s *Syncer) SetMetadata(ctx context.Context, sender types.NodeID, metadata *dstypes.MetadataResponse) {
	s.mtx.RLock()

	if !s.active {
		s.mtx.RUnlock()
		return
	}
	s.mtx.RUnlock()

	if len(metadata.Filenames) != len(metadata.Md5Checksum) {
		s.logger.Error("received bad metadata with inconsistent files and checksums count")
		return
	}

	timedOut, now := s.isCurrentMetadataTimedOut()
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if timedOut {
		if s.fileWorkerCancelFn != nil {
			s.fileWorkerCancelFn()
		}
		state, commit, err := s.commitStateFn(ctx, metadata.Height)
		if err != nil {
			return
		}
		s.state = state
		s.commit = commit
		s.metadataSetAt = now
		s.heightToSync = metadata.Height
		s.expectedChecksums = map[string][]byte{}
		s.syncedFiles = map[string]struct{}{}
		s.pendingFiles = map[string]struct{}{}
		s.completionSignals = map[string]chan struct{}{}
		for i, filename := range metadata.Filenames {
			s.expectedChecksums[filename] = metadata.Md5Checksum[i]
		}
		s.fileQueue = []*dstypes.FileResponse{}
		s.peersToSync = []types.NodeID{sender}
		os.RemoveAll(s.applicationDBDirectory)
		os.MkdirAll(s.applicationDBDirectory, fs.ModeDir)

		cancellableCtx, cancel := context.WithCancel(ctx)
		s.fileWorkerCancelFn = cancel
		s.requestFiles(cancellableCtx, s.metadataSetAt)
	} else if metadata.Height == s.heightToSync {
		s.peersToSync = append(s.peersToSync, sender)
	}
}

func (s *Syncer) Process(ctx context.Context) {
	for {
		s.mtx.RLock()
		if !s.active {
			s.logger.Info(fmt.Sprintf("sync for height %d with %d files finished!", s.heightToSync, len(s.expectedChecksums)))
			s.mtx.RUnlock()
			break
		}
		s.mtx.RUnlock()
		timedOut, _ := s.isCurrentMetadataTimedOut()
		if timedOut {
			s.logger.Info(fmt.Sprintf("last metadata has timed out; sleeping for %d seconds", s.sleepInSeconds))
			s.metadataRequestFn(ctx)
			time.Sleep(s.sleepInSeconds)
			continue
		}
		file := s.popFile()
		if file == nil {
			s.logger.Info(fmt.Sprintf("no file to sync; sleeping for %d seconds", s.sleepInSeconds))
			time.Sleep(s.sleepInSeconds)
			continue
		}
		if err := s.processFile(file); err != nil {
			s.logger.Error(err.Error())
		}
	}
}

func (s *Syncer) processFile(file *dstypes.FileResponse) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	defer func() {
		delete(s.pendingFiles, file.Filename)
	}()

	if file.Height != s.heightToSync {
		return fmt.Errorf("current height is %d but received file for height %d", s.heightToSync, file.Height)
	}

	if expectedChecksum, ok := s.expectedChecksums[file.Filename]; !ok {
		return fmt.Errorf("received unexpected file %s", file.Filename)
	} else if _, ok := s.syncedFiles[file.Filename]; ok {
		return fmt.Errorf("received duplicate file %s", file.Filename)
	} else if _, ok := s.pendingFiles[file.Filename]; !ok {
		return fmt.Errorf("received unrequested file %s", file.Filename)
	} else {
		checkSum := md5.Sum(file.Data)
		if !bytes.Equal(checkSum[:], expectedChecksum) {
			return errors.New("received unexpected checksum")
		}
	}

	dbFile, err := os.Create(path.Join(s.applicationDBDirectory, file.Filename))
	if err != nil {
		return err
	}
	defer dbFile.Close()
	_, err = dbFile.Write(file.Data)
	if err != nil {
		return err
	}

	s.syncedFiles[file.Filename] = struct{}{}
	if len(s.syncedFiles) == len(s.expectedChecksums) {
		// we have finished syncing
		if err := s.postSyncFn(s.state, s.commit); err != nil {
			// no graceful way to handle postsync error since we might be in a partially updated state
			panic(err)
		}
		s.active = false
	}
	s.completionSignals[file.Filename] <- struct{}{}
	return nil
}

func (s *Syncer) isCurrentMetadataTimedOut() (bool, time.Time) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	now := time.Now()
	if s.metadataSetAt.IsZero() {
		return false, now
	}
	return now.After(s.metadataSetAt.Add(time.Second * s.timeoutInSeconds)), now
}

func (s *Syncer) requestFiles(ctx context.Context, metadataSetAt time.Time) {
	worker := func() {
		for {
			s.mtx.Lock()
			if metadataSetAt != s.metadataSetAt {
				s.mtx.Unlock()
				break
			}
			if len(s.expectedChecksums) == len(s.pendingFiles)+len(s.syncedFiles) {
				// even if there are still pending items, there should be enough
				// workers to handle them given one worker can have at most one
				// pending item at a time
				s.mtx.Unlock()
				break
			}
			var picked string
			for filename := range s.expectedChecksums {
				_, pending := s.pendingFiles[filename]
				_, synced := s.syncedFiles[filename]
				if pending || synced {
					continue
				}
				picked = filename
				break
			}
			s.pendingFiles[picked] = struct{}{}
			completionSignal := make(chan struct{})
			s.completionSignals[picked] = completionSignal
			s.mtx.Unlock()

			ticker := time.NewTicker(s.fileWorkerTimeout)
			defer ticker.Stop()

			select {
			case <-completionSignal:

			case <-ticker.C:
				s.mtx.Lock()
				delete(s.pendingFiles, picked)
				s.mtx.Unlock()

			case <-ctx.Done():
				return
			}

			ticker.Stop()
		}
	}
	for i := 0; i < s.fileWorkerCount; i++ {
		go worker()
	}
}

func (s *Syncer) popFile() *dstypes.FileResponse {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if len(s.fileQueue) == 0 {
		return nil
	}

	file := s.fileQueue[0]
	s.fileQueue = s.fileQueue[1:]
	return file
}

func (s *Syncer) PushFile(file *dstypes.FileResponse) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	s.fileQueue = append(s.fileQueue, file)
}
