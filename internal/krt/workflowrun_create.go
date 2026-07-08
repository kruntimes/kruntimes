package krt

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newWorkflowRunCreateCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var filePath string

	cmd := &cobra.Command{
		Use:   "run -f <file>",
		Short: "Create a WorkflowRun from an inline workflow file.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			if filePath == "" {
				return fmt.Errorf("--file is required")
			}

			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("read file %s: %w", filePath, err)
			}

			workflowRun, err := parseWorkflowRun(data, namespace)
			if err != nil {
				return err
			}
			if err := c.Create(cmd.Context(), workflowRun); err != nil {
				return fmt.Errorf("create workflowrun: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "WorkflowRun %s created\n", workflowRun.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&filePath, "file", "f", "", "Path to WorkflowRun or GitHub Actions workflow YAML file (required)")
	cmd.AddCommand(newWorkflowRunListCmd(getter, scheme))
	cmd.AddCommand(newWorkflowRunGetCmd(getter, scheme))
	cmd.AddCommand(newWorkflowRunCancelCmd())
	cmd.AddCommand(newWorkflowRunDeleteCmd(getter, scheme))
	return cmd
}

func newWorkflowRunCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	return newWorkflowRunCreateCmd(getter, scheme)
}

func parseWorkflowRun(data []byte, namespace string) (*v1alpha1.WorkflowRun, error) {
	meta := &metav1.TypeMeta{}
	if err := yaml.Unmarshal(data, meta); err != nil {
		return nil, fmt.Errorf("parse workflowrun metadata: %w", err)
	}
	if meta.Kind == "WorkflowRun" {
		workflowRun := &v1alpha1.WorkflowRun{}
		if err := yaml.Unmarshal(data, workflowRun); err != nil {
			return nil, fmt.Errorf("parse workflowrun: %w", err)
		}
		if workflowRun.Namespace == "" {
			workflowRun.Namespace = namespace
		}
		return workflowRun, nil
	}

	gh := &ghWorkflow{}
	if err := yaml.Unmarshal(data, gh); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if len(gh.Jobs) == 0 {
		return nil, fmt.Errorf("workflowrun file must contain kind: WorkflowRun or GitHub Actions jobs")
	}
	return convertWorkflowRun(gh, namespace), nil
}

func convertWorkflowRun(gh *ghWorkflow, namespace string) *v1alpha1.WorkflowRun {
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "wfr-",
			Namespace:    namespace,
		},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: make(map[string]v1alpha1.JobSpec),
		},
	}

	for jobName, ghJob := range gh.Jobs {
		runsOn := ghJob.RunsOn
		if runsOn == "" {
			runsOn = "bash"
		}
		job := v1alpha1.JobSpec{RunsOn: runsOn, Needs: ghJob.Needs}
		for _, ghStep := range ghJob.Steps {
			step := v1alpha1.StepSpec{
				Name: ghStep.Name,
				Run:  ghStep.Run,
				Env:  ghStep.Env,
				Uses: ghStep.Uses,
				With: ghStep.With,
			}
			job.Steps = append(job.Steps, step)
		}
		workflowRun.Spec.Jobs[jobName] = job
	}

	return workflowRun
}
