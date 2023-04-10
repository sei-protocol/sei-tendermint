package dbsync

import (
	"bytes"
	"crypto/md5"
	"sync"
	"time"

	dstypes "github.com/tendermint/tendermint/proto/tendermint/dbsync"
)

type Syncer struct {
	mtx sync.RWMutex

	active            bool
	heightToSync      uint64
	hashToSync        []byte
	expectedChecksums map[string][]byte
	syncedFiles       map[string]struct{}
	metadataSetAt     time.Time
	timeoutInSeconds  time.Duration
	fileQueue         []*dstypes.FileResponse
}

func NewSyncer(active bool, timeoutInSeconds int) *Syncer {
	return &Syncer{
		active:           active,
		timeoutInSeconds: time.Duration(timeoutInSeconds),
		fileQueue:        []*dstypes.FileResponse{},
	}
}

func (s *Syncer) SetMetadata(metadata *dstypes.MetadataResponse) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if !s.active {
		return
	}

	if len(metadata.Filenames) != len(metadata.Md5Checksum) {
		return
	}

	timedOut, now := s.isCurrentMetadataTimedOut()
	if timedOut {
		s.metadataSetAt = now
		s.heightToSync = metadata.Height
		s.hashToSync = metadata.Hash
		s.expectedChecksums = map[string][]byte{}
		s.syncedFiles = map[string]struct{}{}
		for i, filename := range metadata.Filenames {
			s.expectedChecksums[filename] = metadata.Md5Checksum[i]
		}
		// clear previously sync'ed files
	}
}

func (s *Syncer) processFile(file *dstypes.FileResponse) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	timedOut, _ := s.isCurrentMetadataTimedOut()
	if timedOut {
		return
	}

	if file.Height != s.heightToSync {
		return
	}

	if expectedChecksum, ok := s.expectedChecksums[file.Filename]; !ok {
		return
	} else {
		checkSum := md5.Sum(file.Data)
		if !bytes.Equal(checkSum[:], expectedChecksum) {
			return
		}
	}

}

func (s *Syncer) isCurrentMetadataTimedOut() (bool, time.Time) {
	now := time.Now()
	if s.metadataSetAt.IsZero() {
		return false, now
	}
	return now.After(s.metadataSetAt.Add(time.Second * s.timeoutInSeconds)), now
}
