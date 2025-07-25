package consensus

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tendermint/tendermint/libs/log"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	"github.com/tendermint/tendermint/types"
)

func TestPeerStateMemoryLimits(t *testing.T) {
	logger := log.NewTestingLogger(t)
	peerID := types.NodeID("test-peer")

	testCases := []struct {
		name        string
		total       uint32
		expectError bool
	}{
		{"valid_total", 1, false},
		{"max_valid_total", types.MaxBlockPartsCount, false},
		{"excessive_total", types.MaxBlockPartsCount + 1, true},
		{"very_large_total", 4294967295, true},
	}

	// Test SetHasProposal memory limits
	t.Run("SetHasProposal", func(t *testing.T) {
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				ps := NewPeerState(logger, peerID)
				blockID := types.BlockID{
					Hash: make([]byte, 32),
					PartSetHeader: types.PartSetHeader{
						Total: tc.total,
						Hash:  make([]byte, 32),
					},
				}
				// Create a minimal proposal with basic required fields
				proposal := &types.Proposal{
					Type:      tmproto.ProposalType,
					Height:    1,
					Round:     0,
					POLRound:  -1,
					BlockID:   blockID,
					Timestamp: time.Now(),
					Signature: []byte("test-signature"),
				}
				err := ps.SetHasProposal(proposal)
				if tc.expectError {
					require.Error(t, err, "Expected error for excessive Total")
					require.Contains(t, err.Error(), "too large")
				} else {
					require.NoError(t, err, "Expected no error for valid Total")
				}
			})
		}
	})

	// Test InitProposalBlockParts memory limits
	t.Run("InitProposalBlockParts", func(t *testing.T) {
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				ps := NewPeerState(logger, peerID)
				header := types.PartSetHeader{
					Total: tc.total,
					Hash:  make([]byte, 32),
				}
				ps.InitProposalBlockParts(header)
				if tc.expectError {
					require.Nil(t, ps.PRS.ProposalBlockParts, "Expected ProposalBlockParts to be nil for excessive Total")
				} else {
					require.NotNil(t, ps.PRS.ProposalBlockParts, "Expected ProposalBlockParts to be created")
					require.Equal(t, int(tc.total), ps.PRS.ProposalBlockParts.Size())
					require.Equal(t, header, ps.PRS.ProposalBlockPartSetHeader)
				}
			})
		}
	})
}
