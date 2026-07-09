package krt

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newWorkflowRunDeleteCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <workflowrun-name>",
		Short: "Delete a WorkflowRun.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			workflowRun := &v1alpha1.WorkflowRun{}
			if err := c.Get(cmd.Context(), types.NamespacedName{Name: args[0], Namespace: namespace}, workflowRun); err != nil {
				return fmt.Errorf("get workflowrun: %w", err)
			}
			if err := c.Delete(cmd.Context(), workflowRun); err != nil {
				return fmt.Errorf("delete workflowrun: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "WorkflowRun %s deleted\n", workflowRun.Name)
			return nil
		},
	}
	return cmd
}
