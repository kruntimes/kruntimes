package krt

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
			if ready := apimeta.FindStatusCondition(wf.Status.Conditions, "Ready"); ready != nil {
				fmt.Fprintf(w, "Ready:\t%s\n", ready.Status)
				fmt.Fprintf(w, "Reason:\t%s\n", ready.Reason)
			}
			fmt.Fprintf(w, "Inputs:\t%d\n", len(wf.Spec.Inputs))
			fmt.Fprintf(w, "Outputs:\t%d\n", len(wf.Spec.Outputs))
			fmt.Fprintf(w, "Jobs:\t%d\n", len(wf.Spec.Jobs))
			for jobName, job := range wf.Spec.Jobs {
				if job.Uses != "" {
					fmt.Fprintf(w, "  %s:\t(uses=%s)\n", jobName, job.Uses)
					continue
				}
				fmt.Fprintf(w, "  %s:\t(runs-on=%s, %d steps)\n", jobName, job.RunsOn, len(job.Steps))
				if len(job.Needs) > 0 {
					fmt.Fprintf(w, "    Needs:\t%v\n", job.Needs)
				}
				for _, step := range job.Steps {
					fmt.Fprintf(w, "    Step %s:\trun=%t uses=%s\n", step.Name, step.Run != "", step.Uses)
				}
			}
			fmt.Fprintf(w, "Age:\t%s\n", wf.CreationTimestamp.Format("2006-01-02 15:04:05"))
			return w.Flush()
		},
	}

	addOutputFlag(cmd, &output)
	return cmd
}
