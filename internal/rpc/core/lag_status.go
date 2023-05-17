package core

import (
	"context"
	"fmt"
	"github.com/tendermint/tendermint/rpc/coretypes"
)

// LagStatus returns Tendermint lag status, if lag is over a certain threshold
func (env *Environment) LagStatus(ctx context.Context) (*coretypes.ResultLagStatus, error) {
	latestHeight := env.BlockStore.Height()
	maxPeerBlockHeight := env.BlockSyncReactor.GetMaxPeerBlockHeight()
	lag := int64(0)

	// Calculate lag
	if maxPeerBlockHeight > latestHeight {
		lag = maxPeerBlockHeight - latestHeight
	}

	result := &coretypes.ResultLagStatus{
		LatestHeight:  latestHeight,
		MaxPeerHeight: maxPeerBlockHeight,
		Lag:           lag,
	}

	fmt.Printf("[Tendermint-Debug] Lag is %d and threshold is %d\n", lag, env.Config.LagThreshold)

	// Return a response with error code to differentiate the lagging status by http response code
	if lag > env.Config.LagThreshold {
		return result, coretypes.ErrLagIsTooHigh
	}

	return result, nil
}
