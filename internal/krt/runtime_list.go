package krt

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func NewRuntimeListCmd(c client.Client) *cobra.Command {
	var (
		namespace     string
		allNamespaces bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available runtimes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := namespace
			if allNamespaces {
				ns = ""
			}

			var list v1alpha1.RuntimeList
			if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
				return fmt.Errorf("list runtimes: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tNAMESPACE\tIMAGE\tREPLICAS\tREADY\tPORT\tAGE")
			for _, rt := range list.Items {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
					rt.Name,
					rt.Namespace,
					rt.Spec.Image,
					rt.Spec.Replicas,
					rt.Status.ReadyReplicas,
					rt.Spec.Port,
					rt.CreationTimestamp.Format("2006-01-02 15:04:05"),
				)
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List across all namespaces")
	return cmd
}
