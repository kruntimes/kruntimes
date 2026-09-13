package krt

import (
	"flag"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"

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
	// Keep Kubernetes client diagnostics available to every krt subcommand.
	// A private FlagSet avoids mutating the process-wide flag.CommandLine, so
	// embedding NewRootCmd in tests and other Go programs remains safe.
	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	root.PersistentFlags().AddGoFlagSet(klogFlags)

	runtimeCmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage runtimes.",
	}
	runtimeCmd.AddCommand(newRuntimeListCmd(configFlags, scheme))
	runtimeCmd.AddCommand(newRuntimeGetCmd(configFlags, scheme))

	workflowCmd := &cobra.Command{
		Use:     "workflow",
		Aliases: []string{"wf"},
		Short:   "Manage reusable workflows and workflow runs.",
	}
	workflowCmd.AddCommand(newWorkflowCreateCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowListCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowGetCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowDeleteCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowTriggerCmd(configFlags, scheme))
	workflowCmd.AddCommand(newWorkflowRunCmd(configFlags, scheme))

	root.AddCommand(newRunCmd(configFlags, scheme))
	root.AddCommand(newGetCmd(configFlags, scheme))
	root.AddCommand(newListCmd(configFlags, scheme))
	root.AddCommand(newLogsCmd(configFlags, scheme))
	root.AddCommand(newCancelCmd(configFlags, scheme))
	root.AddCommand(newArtifactCmd(configFlags, scheme))
	root.AddCommand(runtimeCmd)
	root.AddCommand(workflowCmd)
	root.AddCommand(newVersionCmd())

	return root
}
