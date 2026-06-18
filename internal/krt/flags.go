package krt

import "github.com/spf13/cobra"

func addOutputFlag(cmd *cobra.Command, output *string) {
	cmd.Flags().StringVarP(output, "output", "o", outputTable, "Output format: table, json, or yaml")
}
