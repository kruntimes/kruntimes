package krt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func NewLogsCmd(k8sClient client.Client) *cobra.Command {
	var (
		namespace  string
		follow     bool
		tailLines  int
		statusPort int
	)

	cmd := &cobra.Command{
		Use:   "logs <run-name>",
		Short: "Show logs from a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runName := args[0]

			run := &v1alpha1.Run{}
			if err := k8sClient.Get(cmd.Context(), client.ObjectKey{Name: runName, Namespace: namespace}, run); err != nil {
				return fmt.Errorf("get run: %w", err)
			}
			if run.Status.AssignedPod == "" {
				return fmt.Errorf("run %s not yet assigned", runName)
			}

			// Start port-forward.
			localPort := fmt.Sprintf("%d", statusPort)
			remotePort := fmt.Sprintf("%d:9093", statusPort)
			pfCtx, pfCancel := context.WithCancel(cmd.Context())
			defer pfCancel()

			pfCmd := exec.CommandContext(pfCtx, "kubectl",
				"port-forward", run.Status.AssignedPod, remotePort,
				"--namespace", run.Namespace,
			)
			pfCmd.Stderr = os.Stderr
			if err := pfCmd.Start(); err != nil {
				return fmt.Errorf("start port-forward: %w", err)
			}
			defer func() { _ = pfCmd.Process.Kill() }()

			time.Sleep(500 * time.Millisecond)

			target := fmt.Sprintf("localhost:%s", localPort)
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return fmt.Errorf("dial %s: %w", target, err)
			}
			defer conn.Close()

			cli := pb.NewRuntimeClient(conn)
			uid := string(run.UID)

			if !follow {
				return showLogsOnce(cmd.Context(), cli, uid, tailLines)
			}
			return followLogs(cmd.Context(), cli, uid, tailLines)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVar(&tailLines, "tail", 0, "Number of lines from the end of the logs to show")
	cmd.Flags().IntVar(&statusPort, "status-port", 19093, "Local port for port-forward")
	return cmd
}

func showLogsOnce(ctx context.Context, cli pb.RuntimeClient, uid string, tailLines int) error {
	resp, err := cli.Status(ctx, &pb.StatusRequest{Id: uid})
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	output := resp.Stdout
	if output == "" {
		output = resp.Stderr
	}
	out := tailOutput(output, tailLines)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	fmt.Print(out)
	return nil
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

func followLogs(ctx context.Context, cli pb.RuntimeClient, uid string, tailLines int) error {
	// Where to start slicing: 0 = from beginning, or skip first N bytes for tail.
	seen := 0

	if tailLines > 0 {
		resp, err := cli.Status(ctx, &pb.StatusRequest{Id: uid})
		if err == nil {
			tail := tailOutput(resp.Stdout, tailLines)
			if tail != "" && !strings.HasSuffix(tail, "\n") {
				tail += "\n"
			}
			fmt.Print(tail)
			seen = len(resp.Stdout)
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
			time.Sleep(time.Second)
			continue
		}

		if newOut := resp.Stdout[seen:]; newOut != "" {
			fmt.Print(newOut)
			seen = len(resp.Stdout)
		}
		if resp.Stderr != "" {
			fmt.Fprint(os.Stderr, resp.Stderr)
		}

		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED ||
			resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}
