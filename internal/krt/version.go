package krt

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kruntimes/kruntimes/internal/version"
)

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the krt version.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "krt %s\ncommit: %s\nbuilt: %s\n", version.Version, version.Commit, version.Date)
			return err
		},
	}
	return cmd
}
