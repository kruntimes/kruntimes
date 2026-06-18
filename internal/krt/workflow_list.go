package krt

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newWorkflowListCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var (
		allNamespaces bool
		output        string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workflows.",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			ns := namespaceFromConfig(getter)
			if allNamespaces {
				ns = ""
			}

			var list v1alpha1.WorkflowList
			if err := c.List(cmd.Context(), &list, client.InNamespace(ns)); err != nil {
				return fmt.Errorf("list workflows: %w", err)
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, &list)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tNAMESPACE\tPHASE\tAGE")
			for _, wf := range list.Items {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					wf.Name, wf.Namespace, wf.Status.Phase, wf.CreationTimestamp.Format("2006-01-02 15:04:05"))
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List across all namespaces")
	addOutputFlag(cmd, &output)
	return cmd
}
