package krt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newLogsCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var (
		follow     bool
		tailLines  int
		statusPort int
	)

	cmd := &cobra.Command{
		Use:   "logs <run-name>",
		Short: "Show logs from a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sClient, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			restConfig, err := restConfigFromConfig(getter)
			if err != nil {
				return err
			}
			coreClient, err := corev1client.NewForConfig(restConfig)
			if err != nil {
				return fmt.Errorf("create core client: %w", err)
			}
			namespace := namespaceFromConfig(getter)
			runName := args[0]

			run := &v1alpha1.Run{}
			if err := k8sClient.Get(cmd.Context(), client.ObjectKey{Name: runName, Namespace: namespace}, run); err != nil {
				return fmt.Errorf("get run: %w", err)
			}
			if run.Status.AssignedPod == "" {
				return fmt.Errorf("run %s not yet assigned", runName)
			}

			forwarder, err := newPortForwarder(restConfig)
			if err != nil {
				return err
			}
			forward, err := forwarder.Forward(cmd.Context(), run.Namespace, run.Status.AssignedPod, statusPort, artifactRemotePort)
			if err != nil {
				return err
			}
			defer forward.Close()

			target := fmt.Sprintf("127.0.0.1:%d", statusPort)
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return fmt.Errorf("dial %s: %w", target, err)
			}
			defer conn.Close()

			cli := pb.NewRuntimeClient(conn)
			uid := string(run.UID)

			if !follow {
				err := showLogsOnce(cmd.Context(), cli, uid, tailLines, cmd.OutOrStdout(), cmd.ErrOrStderr())
				if isGRPCCode(err, codes.NotFound) {
					return showLogsFromRuntimedPod(cmd.Context(), coreClient, run, tailLines, cmd.OutOrStdout(), cmd.ErrOrStderr())
				}
				return err
			}
			err = followLogs(cmd.Context(), cli, uid, tailLines, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if isGRPCCode(err, codes.NotFound) {
				return showLogsFromRuntimedPod(cmd.Context(), coreClient, run, tailLines, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return err
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVar(&tailLines, "tail", 0, "Number of lines from the end of the logs to show")
	cmd.Flags().IntVar(&statusPort, "status-port", 19093, "Local port for port-forward")
	return cmd
}

func showLogsOnce(
	ctx context.Context,
	cli pb.RuntimeClient,
	uid string,
	tailLines int,
	stdout,
	stderr io.Writer,
) error {
	resp, err := cli.Status(ctx, &pb.StatusRequest{Id: uid})
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if err := writeLogOutput(stdout, tailOutput(resp.Stdout, tailLines)); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	if err := writeLogOutput(stderr, tailOutput(resp.Stderr, tailLines)); err != nil {
		return fmt.Errorf("write stderr: %w", err)
	}
	return nil
}

func writeLogOutput(w io.Writer, output string) error {
	if output == "" {
		return nil
	}
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	_, err := io.WriteString(w, output)
	return err
}

func tailOutput(output string, n int) string {
	if n <= 0 {
		return output
	}
	lines := strings.Split(output, "\n")
	// Trim trailing empty line from split.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func followLogs(
	ctx context.Context,
	cli pb.RuntimeClient,
	uid string,
	tailLines int,
	stdout,
	stderr io.Writer,
) error {
	stdoutOffset := 0
	stderrOffset := 0

	if tailLines > 0 {
		resp, err := cli.Status(ctx, &pb.StatusRequest{Id: uid})
		if err == nil {
			if err := writeLogOutput(stdout, tailOutput(resp.Stdout, tailLines)); err != nil {
				return fmt.Errorf("write stdout: %w", err)
			}
			if err := writeLogOutput(stderr, tailOutput(resp.Stderr, tailLines)); err != nil {
				return fmt.Errorf("write stderr: %w", err)
			}
			stdoutOffset = len(resp.Stdout)
			stderrOffset = len(resp.Stderr)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		resp, err := cli.Status(ctx, &pb.StatusRequest{Id: uid})
		if err != nil {
			if isGRPCCode(err, codes.NotFound) {
				return fmt.Errorf("status: %w", err)
			}
			time.Sleep(time.Second)
			continue
		}

		newStdout, nextStdoutOffset := logOutputSince(resp.Stdout, stdoutOffset)
		if _, err := io.WriteString(stdout, newStdout); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		stdoutOffset = nextStdoutOffset

		newStderr, nextStderrOffset := logOutputSince(resp.Stderr, stderrOffset)
		if _, err := io.WriteString(stderr, newStderr); err != nil {
			return fmt.Errorf("write stderr: %w", err)
		}
		stderrOffset = nextStderrOffset

		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED ||
			resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func logOutputSince(output string, offset int) (string, int) {
	if offset < 0 || offset > len(output) {
		offset = 0
	}
	return output[offset:], len(output)
}

func showLogsFromRuntimedPod(
	ctx context.Context,
	coreClient corev1client.CoreV1Interface,
	run *v1alpha1.Run,
	tailLines int,
	stdout,
	stderr io.Writer,
) error {
	req := coreClient.Pods(run.Namespace).GetLogs(run.Status.AssignedPod, &corev1.PodLogOptions{
		Container: "runtimed",
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("read runtimed logs from pod %s/%s: %w", run.Namespace, run.Status.AssignedPod, err)
	}
	defer stream.Close()
	if err := writeStructuredRunLogs(stream, string(run.UID), tailLines, stdout, stderr); err != nil {
		return fmt.Errorf("read logs for run %s/%s: %w", run.Namespace, run.Name, err)
	}
	return nil
}

type runtimedLogLine struct {
	RunUID  string `json:"run_uid"`
	Stream  string `json:"stream"`
	Message string `json:"message"`
}

func writeStructuredRunLogs(
	reader io.Reader,
	runUID string,
	tailLines int,
	stdout,
	stderr io.Writer,
) error {
	var stdoutLog strings.Builder
	var stderrLog strings.Builder
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var line runtimedLogLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.RunUID != runUID {
			continue
		}
		switch line.Stream {
		case "stdout":
			stdoutLog.WriteString(line.Message)
			stdoutLog.WriteByte('\n')
		case "stderr":
			stderrLog.WriteString(line.Message)
			stderrLog.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := writeLogOutput(stdout, tailOutput(stdoutLog.String(), tailLines)); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	if err := writeLogOutput(stderr, tailOutput(stderrLog.String(), tailLines)); err != nil {
		return fmt.Errorf("write stderr: %w", err)
	}
	return nil
}

func isGRPCCode(err error, code codes.Code) bool {
	for err != nil {
		if status.Code(err) == code {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}
