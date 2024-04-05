package debug

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/internal/store"
	"github.com/tendermint/tendermint/libs/log"
)

func GetBlockStoreCmd(cfg *config.Config, logger log.Logger) *cobra.Command {
	// this will iterate through the block store, load block commit, and dump to file
	cmd := &cobra.Command{
		Use:   "block-store [output-file]",
		Short: "Dump the block store to a file",
		Long:  `Dump the block store to a file. The output file will contain the block height, hash, and commit hash for each block in the block store.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			bsDB, err := config.DefaultDBProvider(&config.DBContext{ID: "blockstore", Config: cfg})
			if err != nil {
				return err
			}
			bs := store.NewBlockStore(bsDB)

			// write latest to a txt file, and then we'll write the 10 most recent block commits
			latestHeight := bs.Height()
			// open a file for writing
			file, err := os.Create(args[0])
			if err != nil {
				return err
			}
			defer file.Close()

			_, err = file.WriteString(fmt.Sprintf("Latest Height: %d\n", latestHeight))
			if err != nil {
				return err
			}
			// write the last 10 block commits marshaled as json to the txt file
			for i := latestHeight; i > latestHeight-10; i-- {
				commit := bs.LoadBlockCommit(i)
				_, err = file.WriteString(fmt.Sprintf("Block Height: %d\n", i))
				if err != nil {
					return err
				}
				if commit == nil {
					_, err = file.WriteString("Block Commit: nil\n\n")
					if err != nil {
						return err
					}
					continue
				}
				_, err = file.WriteString(fmt.Sprintf("Block Hash: %s\n", commit.BlockID.Hash))
				if err != nil {
					return err
				}
				_, err = file.WriteString(fmt.Sprintf("Commit Hash: %s\n\n", commit.Hash()))
				if err != nil {
					return err
				}
				// write signatures
				for _, sig := range commit.Signatures {
					_, err = file.WriteString(fmt.Sprintf("Validator: %s\n", sig.ValidatorAddress))
					if err != nil {
						return err
					}
					_, err = file.WriteString(fmt.Sprintf("Signature: %s\n\n", sig.Signature))
					if err != nil {
						return err
					}
				}
			}

			return nil
		},
	}
	return cmd
}
