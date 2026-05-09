package taskcli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewGetCmd(c client.Client) *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "get <task-name>",
		Short: "Display details of a Task.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task := &v1alpha1.Task{}
			if err := c.Get(context.Background(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, task); err != nil {
				return fmt.Errorf("get task: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Name:\t%s\n", task.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", task.Namespace)
			fmt.Fprintf(w, "Runtime:\t%s\n", task.Spec.Runtime)
			fmt.Fprintf(w, "Phase:\t%s\n", task.Status.Phase)
			fmt.Fprintf(w, "Assigned Pod:\t%s\n", task.Status.AssignedPod)
			fmt.Fprintf(w, "Message:\t%s\n", task.Status.Message)
			fmt.Fprintf(w, "Command:\t%v\n", task.Spec.Commands)
			if task.Status.StartTime != nil {
				fmt.Fprintf(w, "Start Time:\t%s\n", task.Status.StartTime.Format("2006-01-02 15:04:05"))
			}
			if task.Status.CompletionTime != nil {
				fmt.Fprintf(w, "Completion Time:\t%s\n", task.Status.CompletionTime.Format("2006-01-02 15:04:05"))
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	return cmd
}
