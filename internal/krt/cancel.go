package krt

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newCancelCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <run-name>",
		Short: "Cancel a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			run := &v1alpha1.Run{}
			if err := c.Get(cmd.Context(), client.ObjectKey{Name: args[0], Namespace: namespace}, run); err != nil {
				return fmt.Errorf("get run: %w", err)
			}
			run.Spec.CancelRequested = true
			if err := c.Update(cmd.Context(), run); err != nil {
				return fmt.Errorf("cancel run: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cancellation requested for run %s\n", run.Name)
			return nil
		},
	}

	return cmd
}
