package dbsync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/internal/state"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/proto/tendermint/dbsync"
	"github.com/tendermint/tendermint/types"
)

func getTestSyncer(t *testing.T) *Syncer {
	baseConfig := config.DefaultBaseConfig()
	baseConfig.RootDir = t.TempDir()
	dbsyncConfig := config.DefaultDBSyncConfig()
	dbsyncConfig.TimeoutInSeconds = 99999
	syncer := NewSyncer(
		log.NewNopLogger(),
		*dbsyncConfig,
		baseConfig,
		func(ctx context.Context) error { return nil },
		func(ctx context.Context, ni types.NodeID, u uint64, s string) error { return nil },
		func(ctx context.Context, u uint64) (state.State, *types.Commit, error) {
			return state.State{}, nil, nil
		},
		func(ctx context.Context, s state.State, c *types.Commit) error { return nil })
	syncer.active = true
	return syncer
}

func TestSetMetadata(t *testing.T) {
	syncer := getTestSyncer(t)
	// initial
	syncer.SetMetadata(context.Background(), types.NodeID("someone"), &dbsync.MetadataResponse{
		Height:      1,
		Hash:        []byte("hash"),
		Filenames:   []string{"f1"},
		Md5Checksum: [][]byte{[]byte("sum")},
	})
	syncer.fileWorkerCancelFn()
	require.Equal(t, uint64(1), syncer.heightToSync)
	require.NotNil(t, syncer.metadataSetAt)
	require.Equal(t, 1, len(syncer.expectedChecksums))
	require.Equal(t, 1, len(syncer.peersToSync))

	// second time
	syncer.SetMetadata(context.Background(), types.NodeID("someone else"), &dbsync.MetadataResponse{
		Height:      1,
		Hash:        []byte("hash"),
		Filenames:   []string{"f1"},
		Md5Checksum: [][]byte{[]byte("sum")},
	})
	require.Equal(t, uint64(1), syncer.heightToSync)
	require.NotNil(t, syncer.metadataSetAt)
	require.Equal(t, 1, len(syncer.expectedChecksums))
	require.Equal(t, 2, len(syncer.peersToSync))
}
