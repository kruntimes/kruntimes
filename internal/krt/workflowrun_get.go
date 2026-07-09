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

func newWorkflowRunGetCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "get <workflowrun-name>",
		Short: "Display details of a WorkflowRun.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			workflowRun := &v1alpha1.WorkflowRun{}
			if err := c.Get(cmd.Context(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, workflowRun); err != nil {
				return fmt.Errorf("get workflowrun: %w", err)
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, workflowRun)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Name:\t%s\n", workflowRun.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", workflowRun.Namespace)
			fmt.Fprintf(w, "Phase:\t%s\n", workflowRun.Status.Phase)
			if workflowRun.Status.Message != "" {
				fmt.Fprintf(w, "Message:\t%s\n", workflowRun.Status.Message)
			}
			if workflowRun.Spec.Uses != "" {
				fmt.Fprintf(w, "Uses:\t%s\n", workflowRun.Spec.Uses)
				fmt.Fprintf(w, "Inputs:\t%d\n", len(workflowRun.Spec.With))
			}
			fmt.Fprintf(w, "Jobs:\t%d\n", len(workflowRun.Spec.Jobs))
			for jobName, job := range workflowRun.Spec.Jobs {
				if job.Uses != "" {
					fmt.Fprintf(w, "  %s:\t(uses=%s)\n", jobName, job.Uses)
					if len(job.With) > 0 {
						fmt.Fprintf(w, "    Inputs:\t%d\n", len(job.With))
					}
					continue
				}
				fmt.Fprintf(w, "  %s:\t(runs-on=%s, %d steps)\n", jobName, job.RunsOn, len(job.Steps))
				if len(job.Needs) > 0 {
					fmt.Fprintf(w, "    Needs:\t%v\n", job.Needs)
				}
				if js, ok := workflowRun.Status.Jobs[jobName]; ok {
					fmt.Fprintf(w, "    Phase:\t%s\n", js.Phase)
				}
			}
			fmt.Fprintf(w, "Age:\t%s\n", workflowRun.CreationTimestamp.Format("2006-01-02 15:04:05"))
			return w.Flush()
		},
	}

	addOutputFlag(cmd, &output)
	return cmd
}
