package krt

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newWorkflowDeleteCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <workflow-name>",
		Short: "Delete a reusable Workflow definition.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			wf := &v1alpha1.Workflow{}
			if err := c.Get(cmd.Context(), types.NamespacedName{Name: args[0], Namespace: namespace}, wf); err != nil {
				return fmt.Errorf("get workflow: %w", err)
			}
			if err := c.Delete(cmd.Context(), wf); err != nil {
				return fmt.Errorf("delete workflow: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Workflow %s deleted\n", wf.Name)
			return nil
		},
	}
	return cmd
}
