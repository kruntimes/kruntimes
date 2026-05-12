package taskcli

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

	"github.com/airconduct/kruntime/api/v1alpha1"
)

type runOptions struct {
	Runtime   string
	Timeout   time.Duration
	RepoURL   string
	CommitSHA string
	Env       []string
	Wait      bool
	Namespace string
}

func NewRunCmd(c client.Client) *cobra.Command {
	opts := &runOptions{}

	cmd := &cobra.Command{
		Use:   "run --runtime <type> [--wait] [flags] -- <command> [args...]",
		Short: "Create and optionally wait for a Task to complete.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Runtime == "" {
				return fmt.Errorf("--runtime is required")
			}

			task := &v1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("task-%s", rand.String(8)),
					Namespace: opts.Namespace,
				},
				Spec: v1alpha1.TaskSpec{
					Runtime:   opts.Runtime,
					Commands:  []string{strings.Join(args, " ")},
					RepoURL:   opts.RepoURL,
					CommitSHA: opts.CommitSHA,
				},
			}

			if opts.Timeout > 0 {
				task.Spec.Timeout = &metav1.Duration{Duration: opts.Timeout}
			}

			for _, e := range opts.Env {
				parts := strings.SplitN(e, "=", 2)
				ev := corev1.EnvVar{Name: parts[0]}
				if len(parts) == 2 {
					ev.Value = parts[1]
				}
				task.Spec.Env = append(task.Spec.Env, ev)
			}

			ctx := context.Background()
			if err := c.Create(ctx, task); err != nil {
				return fmt.Errorf("create task: %w", err)
			}
			fmt.Printf("Task %s created\n", task.Name)

			if !opts.Wait {
				return nil
			}

			for {
				time.Sleep(500 * time.Millisecond)

				latest := &v1alpha1.Task{}
				if err := c.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
					return fmt.Errorf("get task: %w", err)
				}

				switch latest.Status.Phase {
				case v1alpha1.TaskSucceeded:
					fmt.Println(latest.Status.Message)
					return nil
				case v1alpha1.TaskFailed:
					fmt.Println(latest.Status.Message)
					return fmt.Errorf("task failed")
				case v1alpha1.TaskRunning:
					fmt.Printf("\rRunning on %s...", latest.Status.AssignedPod)
				case v1alpha1.TaskScheduled:
					fmt.Printf("\rScheduled to %s...", latest.Status.AssignedPod)
				}
			}
		},
	}

	cmd.Flags().StringVar(&opts.Runtime, "runtime", "", "Runtime environment type (required)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Task timeout")
	cmd.Flags().StringVar(&opts.RepoURL, "repo-url", "", "Git repository URL")
	cmd.Flags().StringVar(&opts.CommitSHA, "commit-sha", "", "Git commit SHA")
	cmd.Flags().StringArrayVar(&opts.Env, "env", nil, "Environment variables (key=val)")
	cmd.Flags().BoolVar(&opts.Wait, "wait", false, "Block until task completes")
	cmd.Flags().StringVarP(&opts.Namespace, "namespace", "n", "default", "Kubernetes namespace")

	return cmd
}
