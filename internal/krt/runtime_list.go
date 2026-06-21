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

func newRuntimeListCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var (
		allNamespaces bool
		output        string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available runtimes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			ns := namespaceFromConfig(getter)
			if allNamespaces {
				ns = ""
			}

			var list v1alpha1.RuntimeList
			if err := c.List(cmd.Context(), &list, client.InNamespace(ns)); err != nil {
				return fmt.Errorf("list runtimes: %w", err)
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, &list)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tNAMESPACE\tIMAGE\tREPLICAS\tREADY\tPORT\tAGE")
			for _, rt := range list.Items {
				image := "<missing>"
				if len(rt.Spec.Template.Spec.Containers) > 0 {
					image = rt.Spec.Template.Spec.Containers[0].Image
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
					rt.Name,
					rt.Namespace,
					image,
					rt.Spec.Replicas,
					rt.Status.ReadyReplicas,
					rt.Spec.Port,
					rt.CreationTimestamp.Format("2006-01-02 15:04:05"),
				)
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List across all namespaces")
	addOutputFlag(cmd, &output)
	return cmd
}
