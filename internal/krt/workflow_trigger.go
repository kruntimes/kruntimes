package krt

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/workflowtemplate"
)

func newWorkflowTriggerCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var (
		name      string
		inputSets []string
	)

	cmd := &cobra.Command{
		Use:   "trigger <workflow-name>",
		Short: "Trigger a reusable Workflow by creating a WorkflowRun.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			inputs, err := parseWorkflowInputs(inputSets)
			if err != nil {
				return err
			}
			workflowName := args[0]
			workflow := &v1alpha1.Workflow{}
			if err := c.Get(cmd.Context(), client.ObjectKey{Namespace: namespace, Name: workflowName}, workflow); err != nil {
				return fmt.Errorf("get workflow %s: %w", workflowName, err)
			}
			jobs, err := workflowtemplate.Materialize(workflow.Spec, inputs)
			if err != nil {
				return fmt.Errorf("materialize workflow %s: %w", workflowName, err)
			}
			workflowRun := &v1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{
					Name:         name,
					GenerateName: workflowName + "-",
					Namespace:    namespace,
				},
				Spec: v1alpha1.WorkflowRunSpec{
					Jobs: jobs,
				},
			}
			if name != "" {
				workflowRun.GenerateName = ""
			}

			if err := c.Create(cmd.Context(), workflowRun); err != nil {
				return fmt.Errorf("trigger workflow: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "WorkflowRun %s triggered from Workflow %s\n", workflowRun.Name, workflowName)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Name for the created WorkflowRun")
	cmd.Flags().StringArrayVar(&inputSets, "set", nil, "Set a workflow input as key=value; can be repeated")
	return cmd
}

func parseWorkflowInputs(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	inputs := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--set must be key=value")
		}
		inputs[key] = val
	}
	return inputs, nil
}
