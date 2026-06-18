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

func newListCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var (
		allNamespaces bool
		runtime       string
		phase         string
		output        string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Runs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			ns := namespaceFromConfig(getter)
			if allNamespaces {
				ns = ""
			}

			var tasks v1alpha1.RunList
			if err := c.List(cmd.Context(), &tasks, client.InNamespace(ns)); err != nil {
				return fmt.Errorf("list tasks: %w", err)
			}
			tasks.Items = filterRuns(tasks.Items, runtime, phase)
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, &tasks)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tNAMESPACE\tRUNTIME\tPHASE\tASSIGNED POD\tAGE")
			for _, t := range tasks.Items {
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

	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List tasks across all namespaces")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Filter by runtime type")
	cmd.Flags().StringVar(&phase, "phase", "", "Filter by run phase")
	addOutputFlag(cmd, &output)
	return cmd
}

func filterRuns(items []v1alpha1.Run, runtime, phase string) []v1alpha1.Run {
	if runtime == "" && phase == "" {
		return items
	}
	filtered := make([]v1alpha1.Run, 0, len(items))
	for _, item := range items {
		if runtime != "" && item.Spec.Runtime != runtime {
			continue
		}
		if phase != "" && string(item.Status.Phase) != phase {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}
