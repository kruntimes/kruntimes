package krt

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newWorkflowGetCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "get <workflow-name>",
		Short: "Display details of a Workflow.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			wf := &v1alpha1.Workflow{}
			if err := c.Get(cmd.Context(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, wf); err != nil {
				return fmt.Errorf("get workflow: %w", err)
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, wf)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Name:\t%s\n", wf.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", wf.Namespace)
			fmt.Fprintf(w, "Phase:\t%s\n", wf.Status.Phase)
			if wf.Status.Message != "" {
				fmt.Fprintf(w, "Message:\t%s\n", wf.Status.Message)
			}
			fmt.Fprintf(w, "Jobs:\t%d\n", len(wf.Spec.Jobs))
			for jobName, job := range wf.Spec.Jobs {
				js := wf.Status.Jobs[jobName]
				fmt.Fprintf(w, "  %s:\t(runs-on=%s, %d steps, phase=%s)\n", jobName, job.RunsOn, len(job.Steps), js.Phase)
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

	addOutputFlag(cmd, &output)
	return cmd
}
