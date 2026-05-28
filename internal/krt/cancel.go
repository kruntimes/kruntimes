package krt

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func NewCancelCmd(c client.Client) *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "cancel <run-name>",
		Short: "Cancel a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run := &v1alpha1.Run{}
			if err := c.Get(context.Background(), client.ObjectKey{Name: args[0], Namespace: namespace}, run); err != nil {
				return fmt.Errorf("get run: %w", err)
			}
			run.Spec.CancelRequested = true
			if err := c.Update(context.Background(), run); err != nil {
				return fmt.Errorf("cancel run: %w", err)
			}
			fmt.Printf("Cancellation requested for run %s\n", run.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	return cmd
}
