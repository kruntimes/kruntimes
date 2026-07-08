package krt

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newWorkflowRunCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <workflowrun-name>",
		Short: "Cancel a WorkflowRun.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("workflowrun cancellation is not supported yet")
		},
	}
	return cmd
}
