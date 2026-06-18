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
		Short: "Create a Workflow from a GitHub Actions workflow file.",
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

			gh := &ghWorkflow{}
			if err := yaml.Unmarshal(data, gh); err != nil {
				return fmt.Errorf("parse workflow: %w", err)
			}

			wf := convertWorkflow(gh, namespace)
			if err := c.Create(cmd.Context(), wf); err != nil {
				return fmt.Errorf("create workflow: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Workflow %s created\n", wf.Name)
			return nil
		},
	}

	cmd.Flags().StringVarP(&filePath, "file", "f", "", "Path to GitHub Actions workflow YAML file (required)")
	return cmd
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
