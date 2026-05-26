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

func NewWorkflowGetCmd(c client.Client) *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "get <workflow-name>",
		Short: "Display details of a Workflow.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wf := &v1alpha1.Workflow{}
			if err := c.Get(context.Background(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, wf); err != nil {
				return fmt.Errorf("get workflow: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Name:\t%s\n", wf.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", wf.Namespace)
			fmt.Fprintf(w, "Phase:\t%s\n", wf.Status.Phase)
			if wf.Status.Message != "" {
				fmt.Fprintf(w, "Message:\t%s\n", wf.Status.Message)
			}
			fmt.Fprintf(w, "Jobs:\t%d\n", len(wf.Spec.Jobs))
			for _, job := range wf.Spec.Jobs {
				js := wf.Status.Jobs[job.Name]
				fmt.Fprintf(w, "  %s:\t(%d steps, phase=%s)\n", job.Name, len(job.Steps), js.Phase)
				if len(job.Needs) > 0 {
					fmt.Fprintf(w, "    Needs:\t%v\n", job.Needs)
				}
				for _, step := range job.Steps {
					ss := js.Steps[step.Name]
					fmt.Fprintf(w, "    Step %s:\tphase=%s", step.Name, ss.Phase)
					if ss.RunName != "" {
						fmt.Fprintf(w, " run=%s", ss.RunName)
					}
					fmt.Fprintln(w)
				}
			}
			fmt.Fprintf(w, "Age:\t%s\n", wf.CreationTimestamp.Format("2006-01-02 15:04:05"))
			return w.Flush()
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	return cmd
}
