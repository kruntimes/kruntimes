package krt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	artifactv1 "github.com/kruntimes/kruntimes/api/artifact/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const (
	artifactRemotePort       = 9093
	defaultArtifactLocalPort = 19093
	maxArtifactChunkBytes    = 64 * 1024
	runtimeLabel             = "runtime"
)

type artifactDownloader func(
	ctx context.Context,
	podName string,
	run *v1alpha1.Run,
	artifactName, outputPath string,
	localPort int,
) (*artifactv1.ArtifactMetadata, error)

func newArtifactCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	return newArtifactCmdWithConfig(getter, scheme, nil)
}

func newArtifactCmdWithClient(k8sClient client.Client, downloader artifactDownloader) *cobra.Command {
	if downloader == nil {
		downloader = func(
			ctx context.Context,
			podName string,
			run *v1alpha1.Run,
			artifactName, outputPath string,
			localPort int,
		) (*artifactv1.ArtifactMetadata, error) {
			return nil, fmt.Errorf("artifact downloader is required in client-backed tests")
		}
	}
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "List and download Run artifacts.",
	}
	cmd.AddCommand(newArtifactListCmdWithClient(k8sClient))
	cmd.AddCommand(newArtifactDownloadCmdWithClient(k8sClient, downloader))
	return cmd
}

func newArtifactCmdWithConfig(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme, downloader artifactDownloader) *cobra.Command {
	if downloader == nil {
		downloader = func(
			ctx context.Context,
			podName string,
			run *v1alpha1.Run,
			artifactName, outputPath string,
			localPort int,
		) (*artifactv1.ArtifactMetadata, error) {
			k8sClient, err := clientFromConfig(getter, scheme)
			if err != nil {
				return nil, err
			}
			restConfig, err := restConfigFromConfig(getter)
			if err != nil {
				return nil, err
			}
			return DownloadArtifact(ctx, k8sClient, restConfig, run.Namespace, run.Name, artifactName, outputPath, localPort)
		}
	}
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "List and download Run artifacts.",
	}
	cmd.AddCommand(newArtifactListCmd(getter, scheme))
	cmd.AddCommand(newArtifactDownloadCmd(getter, scheme, downloader))
	return cmd
}

func newArtifactListCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list <run>",
		Short: "List artifacts produced by a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sClient, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)
			run, err := getRun(cmd.Context(), k8sClient, namespace, args[0])
			if err != nil {
				return err
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, run.Status.ArtifactRefs)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(writer, "NAME\tTYPE\tSIZE\tCONTENT-TYPE\tDIGEST")
			for _, ref := range run.Status.ArtifactRefs {
				fmt.Fprintf(
					writer,
					"%s\t%s\t%d\t%s\t%s\n",
					ref.Name,
					ref.Type,
					ref.SizeBytes,
					ref.ContentType,
					ref.Digest,
				)
			}
			return writer.Flush()
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newArtifactListCmdWithClient(k8sClient client.Client) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list <run>",
		Short: "List artifacts produced by a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace := "default"
			run, err := getRun(cmd.Context(), k8sClient, namespace, args[0])
			if err != nil {
				return err
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, run.Status.ArtifactRefs)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(writer, "NAME\tTYPE\tSIZE\tCONTENT-TYPE\tDIGEST")
			for _, ref := range run.Status.ArtifactRefs {
				fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\n", ref.Name, ref.Type, ref.SizeBytes, ref.ContentType, ref.Digest)
			}
			return writer.Flush()
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newArtifactDownloadCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme, downloader artifactDownloader) *cobra.Command {
	var (
		output    string
		localPort int
	)
	cmd := &cobra.Command{
		Use:   "download <run> <artifact>",
		Short: "Download an artifact produced by a Run.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sClient, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)
			run, err := getRun(cmd.Context(), k8sClient, namespace, args[0])
			if err != nil {
				return err
			}
			ref, found := findRunArtifact(run, args[1])
			if !found {
				return fmt.Errorf("artifact %q not found on Run %s/%s", args[1], run.Namespace, run.Name)
			}

			pod, err := selectRuntimePod(cmd.Context(), k8sClient, run)
			if err != nil {
				return err
			}
			outputPath := output
			if outputPath == "" {
				outputPath = args[1]
				if ref.Type == v1alpha1.ArtifactTypeDirectory {
					outputPath += ".tar.gz"
				}
			}
			metadata, err := downloader(
				cmd.Context(),
				pod.Name,
				run,
				args[1],
				outputPath,
				localPort,
			)
			if err != nil {
				return err
			}
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"Downloaded %s (%s, %d bytes) to %s\n",
				metadata.GetName(),
				metadata.GetType(),
				metadata.GetSizeBytes(),
				outputPath,
			)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output path (defaults to the artifact name)")
	cmd.Flags().IntVar(&localPort, "status-port", defaultArtifactLocalPort, "Local port for port-forward")
	return cmd
}

func newArtifactDownloadCmdWithClient(k8sClient client.Client, downloader artifactDownloader) *cobra.Command {
	var (
		output    string
		localPort int
	)
	cmd := &cobra.Command{
		Use:   "download <run> <artifact>",
		Short: "Download an artifact produced by a Run.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace := "default"
			run, err := getRun(cmd.Context(), k8sClient, namespace, args[0])
			if err != nil {
				return err
			}
			ref, found := findRunArtifact(run, args[1])
			if !found {
				return fmt.Errorf("artifact %q not found on Run %s/%s", args[1], run.Namespace, run.Name)
			}
			pod, err := selectRuntimePod(cmd.Context(), k8sClient, run)
			if err != nil {
				return err
			}
			outputPath := output
			if outputPath == "" {
				outputPath = args[1]
				if ref.Type == v1alpha1.ArtifactTypeDirectory {
					outputPath += ".tar.gz"
				}
			}
			metadata, err := downloader(cmd.Context(), pod.Name, run, args[1], outputPath, localPort)
			if err != nil {
				return err
			}
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"Downloaded %s (%s, %d bytes) to %s\n",
				metadata.GetName(),
				metadata.GetType(),
				metadata.GetSizeBytes(),
				outputPath,
			)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output path (defaults to the artifact name)")
	cmd.Flags().IntVar(&localPort, "status-port", defaultArtifactLocalPort, "Local port for port-forward")
	return cmd
}

func getRun(ctx context.Context, k8sClient client.Client, namespace, name string) (*v1alpha1.Run, error) {
	run := &v1alpha1.Run{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, run); err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return run, nil
}

func selectRuntimePod(ctx context.Context, k8sClient client.Client, run *v1alpha1.Run) (*corev1.Pod, error) {
	requirement, err := labels.NewRequirement(runtimeLabel, selection.Equals, []string{run.Spec.Runtime})
	if err != nil {
		return nil, fmt.Errorf("build runtime label selector: %w", err)
	}
	pods := &corev1.PodList{}
	if err := k8sClient.List(
		ctx,
		pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*requirement)},
	); err != nil {
		return nil, fmt.Errorf("list runtime pods: %w", err)
	}

	candidates := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if isArtifactAccessPodReady(&pod) {
			candidates = append(candidates, pod)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})
	for i := range candidates {
		if candidates[i].Name == run.Status.AssignedPod {
			return &candidates[i], nil
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no ready runtime pod available for runtime %q", run.Spec.Runtime)
	}
	return &candidates[0], nil
}

func isArtifactAccessPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
		return false
	}
	podReady := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			podReady = condition.Status == corev1.ConditionTrue
			break
		}
	}
	return podReady && runtimepod.IsRuntimedReady(pod)
}

func downloadArtifact(
	ctx context.Context,
	forwarder portForwarder,
	podName string,
	run *v1alpha1.Run,
	artifactName, outputPath string,
	localPort int,
) (*artifactv1.ArtifactMetadata, error) {
	forward, err := forwarder.Forward(ctx, run.Namespace, podName, localPort, artifactRemotePort)
	if err != nil {
		return nil, err
	}
	defer forward.Close()

	target := fmt.Sprintf("127.0.0.1:%d", localPort)
	connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to artifact service: %w", err)
	}
	defer connection.Close()

	return receiveArtifact(
		ctx,
		artifactv1.NewArtifactServiceClient(connection),
		run.Namespace,
		run.Name,
		artifactName,
		outputPath,
	)
}

func DownloadArtifact(
	ctx context.Context,
	k8sClient client.Client,
	restConfig *rest.Config,
	namespace, runName, artifactName, outputPath string,
	localPort int,
) (*artifactv1.ArtifactMetadata, error) {
	run, err := getRun(ctx, k8sClient, namespace, runName)
	if err != nil {
		return nil, err
	}
	pod, err := selectRuntimePod(ctx, k8sClient, run)
	if err != nil {
		return nil, err
	}
	forwarder, err := newPortForwarder(restConfig)
	if err != nil {
		return nil, err
	}
	return downloadArtifact(ctx, forwarder, pod.Name, run, artifactName, outputPath, localPort)
}

func receiveArtifact(
	ctx context.Context,
	artifactClient artifactv1.ArtifactServiceClient,
	namespace, runName, artifactName, outputPath string,
) (_ *artifactv1.ArtifactMetadata, resultErr error) {
	stream, err := artifactClient.Download(ctx, &artifactv1.DownloadRequest{
		Namespace:    namespace,
		RunName:      runName,
		ArtifactName: artifactName,
	})
	if err != nil {
		return nil, fmt.Errorf("start artifact download: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("receive artifact metadata: %w", err)
	}
	metadata := first.GetMetadata()
	if metadata == nil || len(first.GetData()) != 0 {
		return nil, fmt.Errorf("artifact service returned an invalid metadata frame")
	}
	if metadata.GetName() != artifactName {
		return nil, fmt.Errorf("artifact service returned metadata for %q", metadata.GetName())
	}

	outputDir := filepath.Dir(outputPath)
	temp, err := os.CreateTemp(outputDir, "."+filepath.Base(outputPath)+".part-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary output: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		if resultErr != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
		}
	}()

	var written int64
	digest := sha256.New()
	for {
		response, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, fmt.Errorf("receive artifact data: %w", recvErr)
		}
		if response.GetMetadata() != nil {
			return nil, fmt.Errorf("artifact service returned duplicate metadata")
		}
		if len(response.GetData()) > maxArtifactChunkBytes {
			return nil, fmt.Errorf("artifact service returned a %d-byte chunk, maximum is %d", len(response.GetData()), maxArtifactChunkBytes)
		}
		n, writeErr := io.MultiWriter(temp, digest).Write(response.GetData())
		written += int64(n)
		if writeErr != nil {
			return nil, fmt.Errorf("write temporary output: %w", writeErr)
		}
		if n != len(response.GetData()) {
			return nil, io.ErrShortWrite
		}
	}
	if written != metadata.GetSizeBytes() {
		return nil, fmt.Errorf("downloaded %d bytes, expected %d", written, metadata.GetSizeBytes())
	}
	if expected, ok := strings.CutPrefix(metadata.GetDigest(), "sha256:"); ok {
		actual := fmt.Sprintf("%x", digest.Sum(nil))
		if actual != expected {
			return nil, fmt.Errorf("artifact digest mismatch: got sha256:%s, expected %s", actual, metadata.GetDigest())
		}
	}
	if err := temp.Sync(); err != nil {
		return nil, fmt.Errorf("sync temporary output: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close temporary output: %w", err)
	}
	if err := os.Rename(tempName, outputPath); err != nil {
		return nil, fmt.Errorf("publish output: %w", err)
	}
	return metadata, nil
}

func findRunArtifact(run *v1alpha1.Run, name string) (v1alpha1.ArtifactRef, bool) {
	for _, ref := range run.Status.ArtifactRefs {
		if ref.Name == name {
			return ref, true
		}
	}
	return v1alpha1.ArtifactRef{}, false
}
