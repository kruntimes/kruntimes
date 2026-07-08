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

// ghWorkflow is a minimal GitHub Actions workflow format.
type ghWorkflow struct {
	Name string           `yaml:"name"`
	Jobs map[string]ghJob `yaml:"jobs"`
}

type ghJob struct {
	RunsOn string   `yaml:"runs-on"`
	Needs  []string `yaml:"needs"`
	Steps  []ghStep `yaml:"steps"`
}

type ghStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
	Uses string            `yaml:"uses"`
}

func newWorkflowCreateCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var (
		filePath string
	)

	cmd := &cobra.Command{
		Use:   "create -f <file>",
		Short: "Create a reusable Workflow definition from a GitHub Actions workflow file.",
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

			wf, err := parseWorkflow(data, namespace)
			if err != nil {
				return err
			}
			if err := c.Create(cmd.Context(), wf); err != nil {
				return fmt.Errorf("create workflow: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Workflow definition %s created\n", wf.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&filePath, "file", "f", "", "Path to Workflow or GitHub Actions workflow YAML file (required)")
	return cmd
}

func parseWorkflow(data []byte, namespace string) (*v1alpha1.Workflow, error) {
	meta := &metav1.TypeMeta{}
	if err := yaml.Unmarshal(data, meta); err != nil {
		return nil, fmt.Errorf("parse workflow metadata: %w", err)
	}
	if meta.Kind == "Workflow" {
		wf := &v1alpha1.Workflow{}
		if err := yaml.Unmarshal(data, wf); err != nil {
			return nil, fmt.Errorf("parse workflow: %w", err)
		}
		if wf.Namespace == "" {
			wf.Namespace = namespace
		}
		return wf, nil
	}

	gh := &ghWorkflow{}
	if err := yaml.Unmarshal(data, gh); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if len(gh.Jobs) == 0 {
		return nil, fmt.Errorf("workflow file must contain kind: Workflow or GitHub Actions jobs")
	}
	return convertWorkflow(gh, namespace), nil
}

func convertWorkflow(gh *ghWorkflow, namespace string) *v1alpha1.Workflow {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "wf-",
			Namespace:    namespace,
		},
		Spec: v1alpha1.WorkflowSpec{
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
		wf.Spec.Jobs[jobName] = job
	}

	return wf
}
