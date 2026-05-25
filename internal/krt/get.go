package krt

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func NewGetCmd(c client.Client) *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "get <run-name>",
		Short: "Display details of a Run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run := &v1alpha1.Run{}
			if err := c.Get(context.Background(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, run); err != nil {
				return fmt.Errorf("get run: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Name:\t%s\n", run.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", run.Namespace)
			fmt.Fprintf(w, "Runtime:\t%s\n", run.Spec.Runtime)
			fmt.Fprintf(w, "Phase:\t%s\n", run.Status.Phase)
			fmt.Fprintf(w, "Assigned Pod:\t%s\n", run.Status.AssignedPod)
			fmt.Fprintf(w, "Message:\t%s\n", run.Status.Message)
			fmt.Fprintf(w, "Args:\t%v\n", run.Spec.Args)
			if run.Spec.Entrypoint != "" {
				fmt.Fprintf(w, "Entrypoint:\t%s\n", run.Spec.Entrypoint)
			}
			if run.Spec.Handler != "" {
				fmt.Fprintf(w, "Handler:\t%s\n", run.Spec.Handler)
			}
			if run.Spec.Source != nil {
				if run.Spec.Source.Inline != nil {
					fmt.Fprintf(w, "Source Inline:\t%s\n", *run.Spec.Source.Inline)
				}
				if run.Spec.Source.RepoURL != "" {
					fmt.Fprintf(w, "Source Repo:\t%s\n", run.Spec.Source.RepoURL)
				}
				if run.Spec.Source.CommitSHA != "" {
					fmt.Fprintf(w, "Source Commit:\t%s\n", run.Spec.Source.CommitSHA)
				}
			}
			if run.Status.StartTime != nil {
				fmt.Fprintf(w, "Start Time:\t%s\n", run.Status.StartTime.Format("2006-01-02 15:04:05"))
			}
			if run.Status.CompletionTime != nil {
				fmt.Fprintf(w, "Completion Time:\t%s\n", run.Status.CompletionTime.Format("2006-01-02 15:04:05"))
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	return cmd
}
