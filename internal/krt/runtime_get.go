package krt

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func NewRuntimeGetCmd(c client.Client) *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "get <runtime-name>",
		Short: "Display details of a Runtime.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt := &v1alpha1.Runtime{}
			if err := c.Get(context.Background(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, rt); err != nil {
				return fmt.Errorf("get runtime: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Name:\t%s\n", rt.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", rt.Namespace)
			fmt.Fprintf(w, "Image:\t%s\n", rt.Spec.Image)
			fmt.Fprintf(w, "Port:\t%d\n", rt.Spec.Port)
			fmt.Fprintf(w, "Replicas:\t%d\n", rt.Spec.Replicas)
			fmt.Fprintf(w, "Ready:\t%d\n", rt.Status.ReadyReplicas)
			if rt.Spec.DaemonImage != "" {
				fmt.Fprintf(w, "Daemon Image:\t%s\n", rt.Spec.DaemonImage)
			}
			if len(rt.Spec.Command) > 0 {
				fmt.Fprintf(w, "Command:\t%v\n", rt.Spec.Command)
			}
			fmt.Fprintf(w, "Age:\t%s\n", rt.CreationTimestamp.Format("2006-01-02 15:04:05"))
			return w.Flush()
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	return cmd
}
