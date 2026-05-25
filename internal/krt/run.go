package krt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type runOptions struct {
	Runtime    string
	Timeout    time.Duration
	Inline     string
	RepoURL    string
	CommitSHA  string
	Entrypoint string
	Env        []string
	Wait       bool
	Namespace  string
}

func NewRunCmd(c client.Client) *cobra.Command {
	opts := &runOptions{}

	cmd := &cobra.Command{
		Use:   "run --runtime <type> [--wait] [flags] -- <command> [args...]",
		Short: "Create and optionally wait for a Run to complete.",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Runtime == "" {
				return fmt.Errorf("--runtime is required")
			}
			if opts.Inline != "" && opts.RepoURL != "" {
				return fmt.Errorf("--inline and --repo-url are mutually exclusive")
			}

			spec := v1alpha1.RunSpec{
				Runtime:    opts.Runtime,
				Entrypoint: opts.Entrypoint,
				Args:       []string{strings.Join(args, " ")},
			}

			if opts.Inline != "" {
				inline := opts.Inline
				spec.Source = &v1alpha1.CodeSource{Inline: &inline}
			} else if opts.RepoURL != "" {
				spec.Source = &v1alpha1.CodeSource{
					RepoURL:   opts.RepoURL,
					CommitSHA: opts.CommitSHA,
				}
			}

			run := &v1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("run-%s", rand.String(8)),
					Namespace: opts.Namespace,
				},
				Spec: spec,
			}

			if opts.Timeout > 0 {
				run.Spec.Timeout = &metav1.Duration{Duration: opts.Timeout}
			}

			for _, e := range opts.Env {
				parts := strings.SplitN(e, "=", 2)
				ev := corev1.EnvVar{Name: parts[0]}
				if len(parts) == 2 {
					ev.Value = parts[1]
				}
				run.Spec.Env = append(run.Spec.Env, ev)
			}

			ctx := context.Background()
			if err := c.Create(ctx, run); err != nil {
				return fmt.Errorf("create run: %w", err)
			}
			fmt.Printf("Task %s created\n", run.Name)

			if !opts.Wait {
				return nil
			}

			for {
				time.Sleep(500 * time.Millisecond)

				latest := &v1alpha1.Run{}
				if err := c.Get(ctx, types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, latest); err != nil {
					return fmt.Errorf("get run: %w", err)
				}

				switch latest.Status.Phase {
				case v1alpha1.RunSucceeded:
					fmt.Println(latest.Status.Message)
					return nil
				case v1alpha1.RunFailed:
					fmt.Println(latest.Status.Message)
					return fmt.Errorf("run failed")
				case v1alpha1.RunRunning:
					fmt.Printf("\rRunning on %s...", latest.Status.AssignedPod)
				case v1alpha1.RunScheduled:
					fmt.Printf("\rScheduled to %s...", latest.Status.AssignedPod)
				}
			}
		},
	}

	cmd.Flags().StringVar(&opts.Runtime, "runtime", "", "Runtime environment type (required)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Task timeout")
	cmd.Flags().StringVar(&opts.Inline, "inline", "", "Inline source code to execute")
	cmd.Flags().StringVar(&opts.Entrypoint, "entrypoint", "", "Entrypoint in module.function format")
	cmd.Flags().StringVar(&opts.RepoURL, "repo-url", "", "Git repository URL")
	cmd.Flags().StringVar(&opts.CommitSHA, "commit-sha", "", "Git commit SHA")
	cmd.Flags().StringArrayVar(&opts.Env, "env", nil, "Environment variables (key=val)")
	cmd.Flags().BoolVar(&opts.Wait, "wait", false, "Block until run completes")
	cmd.Flags().StringVarP(&opts.Namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}
