package main

import (
	"fmt"
	"os"

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/airconduct/kruntime/internal/taskcli"
	"github.com/spf13/cobra"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	utilruntime.Must(v1alpha1.AddToScheme(scheme.Scheme))

	restConfig := ctrl.GetConfigOrDie()
	c, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}

	root := &cobra.Command{
		Use:   "task-cli",
		Short: "CLI for interacting with kruntime Task CRDs.",
	}

	root.AddCommand(taskcli.NewRunCmd(c))
	root.AddCommand(taskcli.NewGetCmd(c))
	root.AddCommand(taskcli.NewListCmd(c))

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
