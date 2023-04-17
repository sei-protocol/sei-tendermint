package commands

import (
	"strconv"

	"github.com/spf13/cobra"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/internal/dbsync"
)

func MakeSnapshotCommand(conf *config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "snapshot [height]",
		Short: "Take DBSync snapshot for given height",
		RunE: func(cmd *cobra.Command, args []string) error {
			height, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return err
			}
			return dbsync.Snapshot(height, *conf.DBSync, conf.BaseConfig)
		},
	}
}
