package krt

import (
	"fmt"
	"io"
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

			return writeRunListTable(cmd.OutOrStdout(), &tasks)
		},
	}

	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List tasks across all namespaces")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Filter by runtime type")
	cmd.Flags().StringVar(&phase, "phase", "", "Filter by run phase")
	addOutputFlag(cmd, &output)
	return cmd
}

func writeRunListTable(output io.Writer, runs *v1alpha1.RunList) error {
	w := tabwriter.NewWriter(output, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tNAMESPACE\tRUNTIME\tMODE\tPHASE\tASSIGNED POD\tAGE"); err != nil {
		return err
	}
	for _, run := range runs.Items {
		age := ""
		if run.CreationTimestamp.Unix() > 0 {
			age = fmt.Sprintf("%v", run.CreationTimestamp.Time)
		}
		assignedPod := run.Status.AssignedPod
		if assignedPod == "" {
			assignedPod = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			run.Name, run.Namespace, run.Spec.Runtime, run.Spec.Mode.Name(), run.Status.Phase, assignedPod, age); err != nil {
			return err
		}
	}
	return w.Flush()
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
