package krt

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func newRuntimeGetCmd(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "get <runtime-name>",
		Short: "Display details of a Runtime.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromConfig(getter, scheme)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)

			rt := &v1alpha1.Runtime{}
			if err := c.Get(cmd.Context(), types.NamespacedName{
				Name: args[0], Namespace: namespace,
			}, rt); err != nil {
				return fmt.Errorf("get runtime: %w", err)
			}
			if output != outputTable {
				return writeStructuredOutput(cmd.OutOrStdout(), output, rt)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			var runtimeContainer *corev1.Container
			if len(rt.Spec.Template.Spec.Containers) > 0 {
				runtimeContainer = &rt.Spec.Template.Spec.Containers[0]
			}
			fmt.Fprintf(w, "Name:\t%s\n", rt.Name)
			fmt.Fprintf(w, "Namespace:\t%s\n", rt.Namespace)
			if runtimeContainer != nil {
				fmt.Fprintf(w, "Image:\t%s\n", runtimeContainer.Image)
			}
			fmt.Fprintf(w, "Port:\t%d\n", rt.Spec.Port)
			fmt.Fprintf(w, "Replicas:\t%d\n", rt.Spec.Replicas)
			fmt.Fprintf(w, "Ready:\t%d\n", rt.Status.ReadyReplicas)
			if rt.Spec.DaemonImage != "" {
				fmt.Fprintf(w, "Daemon Image:\t%s\n", rt.Spec.DaemonImage)
			}
			if rt.Spec.Template.Spec.ServiceAccountName != "" {
				fmt.Fprintf(w, "ServiceAccount:\t%s\n", rt.Spec.Template.Spec.ServiceAccountName)
			}
			if runtimeContainer != nil && len(runtimeContainer.Command) > 0 {
				fmt.Fprintf(w, "Command:\t%v\n", runtimeContainer.Command)
			}
			if runtimeContainer != nil && len(runtimeContainer.Args) > 0 {
				fmt.Fprintf(w, "Args:\t%v\n", runtimeContainer.Args)
			}
			fmt.Fprintf(w, "Age:\t%s\n", rt.CreationTimestamp.Format("2006-01-02 15:04:05"))
			return w.Flush()
		},
	}

	addOutputFlag(cmd, &output)
	return cmd
}
