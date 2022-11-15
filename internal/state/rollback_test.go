package state_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	dbm "github.com/tendermint/tm-db"

	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/crypto/tmhash"
	"github.com/tendermint/tendermint/internal/state"
	"github.com/tendermint/tendermint/internal/state/mocks"
	"github.com/tendermint/tendermint/internal/store"
	"github.com/tendermint/tendermint/internal/test/factory"
	"github.com/tendermint/tendermint/types"
	"github.com/tendermint/tendermint/version"
)

func TestRollback(t *testing.T) {
	var (
		height     int64 = 100
		nextHeight int64 = 101
	)

	blockStore := &mocks.BlockStore{}
	stateStore := setupStateStore(t, height)
	initialState, err := stateStore.Load()
	require.NoError(t, err)

	// perform the rollback over a version bump
	newParams := types.DefaultConsensusParams()
	newParams.Version.AppVersion = 11
	newParams.Block.MaxBytes = 1000
	nextState := initialState.Copy()
	nextState.LastBlockHeight = nextHeight
	nextState.Version.Consensus.App = 11
	nextState.LastBlockID = factory.MakeBlockID()
	nextState.AppHash = factory.RandomHash()
	nextState.LastValidators = initialState.Validators
	nextState.Validators = initialState.NextValidators
	nextState.NextValidators = initialState.NextValidators.CopyIncrementProposerPriority(1)
	nextState.ConsensusParams = *newParams
	nextState.LastHeightConsensusParamsChanged = nextHeight + 1
	nextState.LastHeightValidatorsChanged = nextHeight + 1

	// update the state
	require.NoError(t, stateStore.Save(nextState))

	block := &types.BlockMeta{
		BlockID: initialState.LastBlockID,
		Header: types.Header{
			Height:          initialState.LastBlockHeight,
			AppHash:         factory.RandomHash(),
			LastBlockID:     factory.MakeBlockID(),
			LastResultsHash: initialState.LastResultsHash,
		},
	}
	nextBlock := &types.BlockMeta{
		BlockID: initialState.LastBlockID,
		Header: types.Header{
			Height:          nextState.LastBlockHeight,
			AppHash:         initialState.AppHash,
			LastBlockID:     block.BlockID,
			LastResultsHash: nextState.LastResultsHash,
		},
	}
	blockStore.On("LoadBlockMeta", height).Return(block)
	blockStore.On("LoadBlockMeta", nextHeight).Return(nextBlock)
	blockStore.On("Height").Return(nextHeight)

	// rollback the state
	rollbackHeight, rollbackHash, err := state.Rollback(blockStore, stateStore, false)
	require.NoError(t, err)
	require.EqualValues(t, height, rollbackHeight)
	require.EqualValues(t, initialState.AppHash, rollbackHash)
	blockStore.AssertExpectations(t)

	// assert that we've recovered the prior state
	loadedState, err := stateStore.Load()
	require.NoError(t, err)
	require.EqualValues(t, initialState, loadedState)
}

func TestRollbackNoState(t *testing.T) {
	stateStore := state.NewStore(dbm.NewMemDB())
	blockStore := &mocks.BlockStore{}

	_, _, err := state.Rollback(blockStore, stateStore, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no state found")
}

func TestRollbackNoBlocks(t *testing.T) {
	const height = int64(100)

	stateStore := setupStateStore(t, height)
	blockStore := &mocks.BlockStore{}
	blockStore.On("Height").Return(height)
	blockStore.On("LoadBlockMeta", height).Return(nil)
	blockStore.On("LoadBlockMeta", height-1).Return(nil)

	_, _, err := state.Rollback(blockStore, stateStore, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "block at height 99 not found")
}

func TestRollbackDifferentStateHeight(t *testing.T) {
	const height = int64(100)
	stateStore := setupStateStore(t, height)
	blockStore := &mocks.BlockStore{}
	blockStore.On("Height").Return(height + 2)

	_, _, err := state.Rollback(blockStore, stateStore, false)
	require.Error(t, err)
	require.Equal(t, err.Error(), "statestore height (100) is not one below or equal to blockstore height (102)")
}

func setupStateStore(t *testing.T, height int64) state.Store {
	stateStore := state.NewStore(dbm.NewMemDB())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	valSet, _ := factory.ValidatorSet(ctx, t, 5, 10)

	params := types.DefaultConsensusParams()
	params.Version.AppVersion = 10

	initialState := state.State{
		Version: state.Version{
			Consensus: version.Consensus{
				Block: version.BlockProtocol,
				App:   10,
			},
			Software: version.TMVersion,
		},
		ChainID:                          factory.DefaultTestChainID,
		InitialHeight:                    10,
		LastBlockID:                      factory.MakeBlockID(),
		AppHash:                          factory.RandomHash(),
		LastResultsHash:                  factory.RandomHash(),
		LastBlockHeight:                  height,
		LastValidators:                   valSet,
		Validators:                       valSet.CopyIncrementProposerPriority(1),
		NextValidators:                   valSet.CopyIncrementProposerPriority(2),
		LastHeightValidatorsChanged:      height + 1,
		ConsensusParams:                  *params,
		LastHeightConsensusParamsChanged: height + 1,
	}
	require.NoError(t, stateStore.Bootstrap(initialState))
	return stateStore
}

func TestRollbackHard(t *testing.T) {
	const height int64 = 100
	blockStore := store.NewBlockStore(dbm.NewMemDB())
	stateStore := state.NewStore(dbm.NewMemDB())

	valSet, _ := types.RandValidatorSet(5, 10)

	params := types.DefaultConsensusParams()
	params.Version.AppVersion = 10
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	block := &types.Block{
		Header: types.Header{
			Version:            version.Consensus{Block: version.BlockProtocol, App: 1},
			ChainID:            "test-chain",
			Time:               now,
			Height:             height,
			AppHash:            crypto.CRandBytes(tmhash.Size),
			LastBlockID:        makeBlockIDRandom(),
			LastCommitHash:     crypto.CRandBytes(tmhash.Size),
			DataHash:           crypto.CRandBytes(tmhash.Size),
			ValidatorsHash:     valSet.Hash(),
			NextValidatorsHash: valSet.CopyIncrementProposerPriority(1).Hash(),
			ConsensusHash:      params.HashConsensusParams(),
			LastResultsHash:    crypto.CRandBytes(tmhash.Size),
			EvidenceHash:       crypto.CRandBytes(tmhash.Size),
			ProposerAddress:    crypto.CRandBytes(crypto.AddressSize),
		},
		LastCommit: &types.Commit{Height: height - 1},
	}

	partSet, err := block.MakePartSet(types.BlockPartSizeBytes)
	require.NoError(t, err)
	blockStore.SaveBlock(block, partSet, &types.Commit{Height: block.Height})

	currState := state.State{
		Version: state.Version{
			Consensus: block.Header.Version,
			Software:  version.TMVersion,
		},
		LastBlockHeight:                  block.Height,
		LastBlockTime:                    block.Time,
		AppHash:                          crypto.CRandBytes(tmhash.Size),
		LastValidators:                   valSet,
		Validators:                       valSet.CopyIncrementProposerPriority(1),
		NextValidators:                   valSet.CopyIncrementProposerPriority(2),
		ConsensusParams:                  *params,
		LastHeightConsensusParamsChanged: height + 1,
		LastHeightValidatorsChanged:      height + 1,
		LastResultsHash:                  crypto.CRandBytes(tmhash.Size),
	}
	require.NoError(t, stateStore.Bootstrap(currState))

	nextBlock := &types.Block{
		Header: types.Header{
			Version:            version.Consensus{Block: version.BlockProtocol, App: 1},
			ChainID:            block.ChainID,
			Time:               block.Time,
			Height:             currState.LastBlockHeight + 1,
			AppHash:            currState.AppHash,
			LastBlockID:        types.BlockID{Hash: block.Hash(), PartSetHeader: partSet.Header()},
			LastCommitHash:     crypto.CRandBytes(tmhash.Size),
			DataHash:           crypto.CRandBytes(tmhash.Size),
			ValidatorsHash:     valSet.CopyIncrementProposerPriority(1).Hash(),
			NextValidatorsHash: valSet.CopyIncrementProposerPriority(2).Hash(),
			ConsensusHash:      params.HashConsensusParams(),
			LastResultsHash:    currState.LastResultsHash,
			EvidenceHash:       crypto.CRandBytes(tmhash.Size),
			ProposerAddress:    crypto.CRandBytes(crypto.AddressSize),
		},
		LastCommit: &types.Commit{Height: currState.LastBlockHeight},
	}

	nextPartSet, err := nextBlock.MakePartSet(types.BlockPartSizeBytes)
	require.NoError(t, err)
	blockStore.SaveBlock(nextBlock, nextPartSet, &types.Commit{Height: nextBlock.Height})

	rollbackHeight, rollbackHash, err := state.Rollback(blockStore, stateStore, true)
	require.NoError(t, err)
	require.Equal(t, rollbackHeight, currState.LastBlockHeight)
	require.Equal(t, rollbackHash, currState.AppHash)

	// state should not have been changed
	loadedState, err := stateStore.Load()
	require.NoError(t, err)
	require.Equal(t, currState, loadedState)

	// resave the same block
	blockStore.SaveBlock(nextBlock, nextPartSet, &types.Commit{Height: nextBlock.Height})

	params.Version.AppVersion = 11

	nextState := state.State{
		Version: state.Version{
			Consensus: block.Header.Version,
			Software:  version.TMVersion,
		},
		LastBlockHeight:                  nextBlock.Height,
		LastBlockTime:                    nextBlock.Time,
		AppHash:                          crypto.CRandBytes(tmhash.Size),
		LastValidators:                   valSet.CopyIncrementProposerPriority(1),
		Validators:                       valSet.CopyIncrementProposerPriority(2),
		NextValidators:                   valSet.CopyIncrementProposerPriority(3),
		ConsensusParams:                  *params,
		LastHeightConsensusParamsChanged: nextBlock.Height + 1,
		LastHeightValidatorsChanged:      nextBlock.Height + 1,
		LastResultsHash:                  crypto.CRandBytes(tmhash.Size),
	}
	require.NoError(t, stateStore.Save(nextState))

	rollbackHeight, rollbackHash, err = state.Rollback(blockStore, stateStore, true)
	require.NoError(t, err)
	require.Equal(t, rollbackHeight, currState.LastBlockHeight)
	require.Equal(t, rollbackHash, currState.AppHash)
}

func makeBlockIDRandom() types.BlockID {
	var (
		blockHash   = make([]byte, tmhash.Size)
		partSetHash = make([]byte, tmhash.Size)
	)
	rand.Read(blockHash)   //nolint: errcheck // ignore errcheck for read
	rand.Read(partSetHash) //nolint: errcheck // ignore errcheck for read
	return types.BlockID{
		Hash: blockHash,
		PartSetHeader: types.PartSetHeader{
			Total: 123,
			Hash:  partSetHash,
		},
	}
}
