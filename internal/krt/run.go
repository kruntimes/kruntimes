package krt

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type runOptions struct {
	Runtime       string
	Timeout       time.Duration
	File          string
	RepoURL       string
	CommitSHA     string
	Entrypoint    string
	Handler       string
	Env           []string
	Wait          bool
	RetryAttempts int32
	RetryBackoff  time.Duration
}

func newRunCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	opts := &runOptions{}

	cmd := &cobra.Command{
		Use:   "run --runtime <type> [--wait] [flags] -- <command> [args...]",
		Short: "Create and optionally wait for a Run to complete.",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			if opts.File != "" && opts.RepoURL != "" {
				return fmt.Errorf("-f/--file and --repo-url are mutually exclusive")
			}
			if err := validateRunInputOptions(opts, args); err != nil {
				return err
			}

			var inline string
			if opts.File != "" {
				var data []byte
				var err error
				if opts.File == "-" {
					data, err = io.ReadAll(os.Stdin)
				} else {
					data, err = os.ReadFile(opts.File)
				}
				if err != nil {
					return fmt.Errorf("read file: %w", err)
				}
				inline = string(data)
			}

			spec := v1alpha1.RunSpec{
				Runtime: opts.Runtime,
				Mode: v1alpha1.RunMode{
					Task: &v1alpha1.RunTaskMode{
						Entrypoint: opts.Entrypoint,
						Args:       args,
					},
				},
			}
			if opts.Handler != "" {
				spec.Mode = v1alpha1.RunMode{
					Function: &v1alpha1.RunFunctionMode{
						Handler: opts.Handler,
					},
				}
			}

			if inline != "" {
				spec.Source = &v1alpha1.CodeSource{Inline: &inline}
			} else if opts.RepoURL != "" {
				spec.Source = &v1alpha1.CodeSource{
					RepoURL:   opts.RepoURL,
					CommitSHA: opts.CommitSHA,
				}
			}

			if opts.RetryAttempts > 0 {
				backoff := opts.RetryBackoff
				if backoff <= 0 {
					backoff = time.Second
				}
				spec.RetryPolicy = &v1alpha1.RetryPolicy{
					MaxAttempts: opts.RetryAttempts,
					Backoff:     metav1.Duration{Duration: backoff},
				}
			}

			run := &v1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("run-%s", rand.String(8)),
					Namespace: namespace,
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

			ctx := cmd.Context()
			if err := c.Create(ctx, run); err != nil {
				return fmt.Errorf("create run: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Task %s created\n", run.Name)

			if !opts.Wait {
				return nil
			}

			for {
				time.Sleep(500 * time.Millisecond)

				latest := &v1alpha1.Run{}
				if err := c.Get(ctx, types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, latest); err != nil {
					return fmt.Errorf("get run: %w", err)
				}

				if done, err := runTerminalResult(latest.Status.Phase); done {
					fmt.Fprintln(cmd.OutOrStdout(), latest.Status.Message)
					return err
				}

				switch latest.Status.Phase {
				case v1alpha1.RunRunning:
					fmt.Fprintf(cmd.OutOrStdout(), "\rRunning on %s...", latest.Status.AssignedPod)
				case v1alpha1.RunReady:
					fmt.Fprintf(cmd.OutOrStdout(), "\rReady on %s...", latest.Status.AssignedPod)
				case v1alpha1.RunScheduled:
					fmt.Fprintf(cmd.OutOrStdout(), "\rScheduled to %s...", latest.Status.AssignedPod)
				}
			}
		},
	}

	cmd.Flags().StringVarP(&opts.Runtime, "runtime", "r", "bash", "Runtime environment type")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Task timeout")
	cmd.Flags().StringVarP(&opts.File, "file", "f", "", `Read source code from file, or "-" for stdin`)
	cmd.Flags().StringVar(&opts.Entrypoint, "entrypoint", "", "Entrypoint script file for non-inline sources")
	cmd.Flags().StringVar(&opts.Handler, "handler", "", "Handler in module.function format")
	cmd.Flags().StringVar(&opts.RepoURL, "repo-url", "", "Git repository URL")
	cmd.Flags().StringVar(&opts.CommitSHA, "commit-sha", "", "Git commit SHA")
	cmd.Flags().StringArrayVar(&opts.Env, "env", nil, "Environment variables (key=val)")
	cmd.Flags().BoolVar(&opts.Wait, "wait", false, "Block until run completes")
	cmd.Flags().Int32Var(&opts.RetryAttempts, "retry-attempts", 0, "Maximum execution attempts (including initial attempt)")
	cmd.Flags().DurationVar(&opts.RetryBackoff, "retry-backoff", 0, "Initial backoff between retries")

	return cmd
}

func validateRunInputOptions(opts *runOptions, args []string) error {
	if opts.Handler != "" {
		if opts.Entrypoint != "" {
			return fmt.Errorf("--entrypoint cannot be used with --handler")
		}
		if len(args) > 0 {
			return fmt.Errorf("command args cannot be used with --handler")
		}
	}
	if opts.File == "" {
		return nil
	}
	if opts.Entrypoint != "" {
		return fmt.Errorf("--entrypoint cannot be used with --file because inline source uses the default script entrypoint")
	}
	if len(args) > 0 {
		return fmt.Errorf("command args cannot be used with --file because inline source ignores args")
	}
	return nil
}

func runTerminalResult(phase v1alpha1.RunPhase) (bool, error) {
	switch phase {
	case v1alpha1.RunSucceeded:
		return true, nil
	case v1alpha1.RunFailed:
		return true, fmt.Errorf("run failed")
	case v1alpha1.RunTimeout:
		return true, fmt.Errorf("run timed out")
	case v1alpha1.RunCancelled:
		return true, fmt.Errorf("run cancelled")
	default:
		return false, nil
	}
}
