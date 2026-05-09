package taskcli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewListCmd(c client.Client) *cobra.Command {
	var (
		namespace     string
		allNamespaces bool
		runtime       string
		phase         string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Tasks.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := namespace
			if allNamespaces {
				ns = ""
			}

			var tasks v1alpha1.TaskList
			if err := c.List(context.Background(), &tasks, client.InNamespace(ns)); err != nil {
				return fmt.Errorf("list tasks: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tNAMESPACE\tRUNTIME\tPHASE\tASSIGNED POD\tAGE")
			for _, t := range tasks.Items {
				if runtime != "" && t.Spec.Runtime != runtime {
					continue
				}
				if phase != "" && string(t.Status.Phase) != phase {
					continue
				}
				age := ""
				if t.CreationTimestamp.Time.Unix() > 0 {
					age = fmt.Sprintf("%v", t.CreationTimestamp.Time)
				}
				assignedPod := t.Status.AssignedPod
				if assignedPod == "" {
					assignedPod = "-"
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.Name, t.Namespace, t.Spec.Runtime, t.Status.Phase, assignedPod, age)
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List tasks across all namespaces")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Filter by runtime type")
	cmd.Flags().StringVar(&phase, "phase", "", "Filter by task phase")
	return cmd
}
