package krt

import (
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func NewRootCmd() *cobra.Command {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	configFlags := genericclioptions.NewConfigFlags(true)

	root := &cobra.Command{
		Use:   "krt",
		Short: "CLI for interacting with kruntimes Run CRDs.",
	}
	configFlags.AddFlags(root.PersistentFlags())

	runtimeCmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage runtimes.",
	}
	runtimeCmd.AddCommand(newRuntimeListCmd(configFlags, scheme))
	runtimeCmd.AddCommand(newRuntimeGetCmd(configFlags, scheme))

	workflowCmd := &cobra.Command{
		Use:   "workflow",
		Short: "Manage workflows.",
	}
	workflowCmd.AddCommand(newWorkflowCreateCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowListCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowGetCmd(configFlags, scheme))

	root.AddCommand(newRunCmd(configFlags, scheme))
	root.AddCommand(newGetCmd(configFlags, scheme))
	root.AddCommand(newListCmd(configFlags, scheme))
	root.AddCommand(newLogsCmd(configFlags, scheme))
	root.AddCommand(newCancelCmd(configFlags, scheme))
	root.AddCommand(newArtifactCmd(configFlags, scheme))
	root.AddCommand(runtimeCmd)
	root.AddCommand(workflowCmd)

	return root
}
