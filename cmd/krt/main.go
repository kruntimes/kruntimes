package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	restConfig := ctrl.GetConfigOrDie()
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}

	root := &cobra.Command{
		Use:   "krt",
		Short: "CLI for interacting with kruntimes Run CRDs.",
	}

	runtimeCmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage runtimes.",
	}
	runtimeCmd.AddCommand(krt.NewRuntimeListCmd(c))
	runtimeCmd.AddCommand(krt.NewRuntimeGetCmd(c))

	workflowCmd := &cobra.Command{
		Use:   "workflow",
		Short: "Manage workflows.",
	}
	workflowCmd.AddCommand(krt.NewWorkflowCreateCmd(c))
	workflowCmd.AddCommand(krt.NewWorkflowListCmd(c))
	workflowCmd.AddCommand(krt.NewWorkflowGetCmd(c))

	root.AddCommand(krt.NewRunCmd(c))
	root.AddCommand(krt.NewGetCmd(c))
	root.AddCommand(krt.NewListCmd(c))
	root.AddCommand(krt.NewLogsCmd(c))
	root.AddCommand(krt.NewCancelCmd(c))
	root.AddCommand(runtimeCmd)
	root.AddCommand(workflowCmd)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
