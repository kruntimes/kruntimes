package krt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
		namespace string
		follow    bool
		statusPort int
	)

	cmd := &cobra.Command{
		Use:   "logs <run-name>",
		Short: "Stream logs from a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runName := args[0]

			// Get the Run to find assigned pod and UID.
			run := &v1alpha1.Run{}
			if err := k8sClient.Get(cmd.Context(), client.ObjectKey{Name: runName, Namespace: namespace}, run); err != nil {
				return fmt.Errorf("get run: %w", err)
			}
			if run.Status.AssignedPod == "" {
				return fmt.Errorf("run %s not yet assigned", runName)
			}

			// Start port-forward to the pod's status proxy.
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
			defer pfCmd.Process.Kill()

			// Wait for port-forward to be ready.
			time.Sleep(500 * time.Millisecond)

			// Connect gRPC client to the forwarded port.
			target := fmt.Sprintf("localhost:%s", localPort)
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return fmt.Errorf("dial %s: %w", target, err)
			}
			defer conn.Close()

			cli := pb.NewRuntimeClient(conn)
			uid := string(run.UID)

			var lastStdout string
			for {
				select {
				case <-cmd.Context().Done():
					return nil
				default:
				}

				resp, err := cli.Status(cmd.Context(), &pb.StatusRequest{Id: uid})
				if err != nil {
					// If the run isn't on this pod, the runtime server won't know about it.
					// This is expected while the run hasn't reached this pod yet.
					if !follow {
						return fmt.Errorf("status: %w", err)
					}
					time.Sleep(time.Second)
					continue
				}

				// Print new stdout since last poll.
				if newOut := resp.Stdout[len(lastStdout):]; newOut != "" {
					fmt.Print(newOut)
					lastStdout = resp.Stdout
				}
				// Print stderr if present and new.
				if resp.Stderr != "" {
					fmt.Fprint(os.Stderr, resp.Stderr)
				}

				if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED ||
					resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
					return nil
				}

				time.Sleep(500 * time.Millisecond)
			}
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "Follow log output")
	cmd.Flags().IntVar(&statusPort, "status-port", 19093, "Local port for port-forward")
	return cmd
}
