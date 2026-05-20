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

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/airconduct/kruntime/internal/taskcli"
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
		Use:   "run-cli",
		Short: "CLI for interacting with kruntime Run CRDs.",
	}

	root.AddCommand(taskcli.NewRunCmd(c))
	root.AddCommand(taskcli.NewGetCmd(c))
	root.AddCommand(taskcli.NewListCmd(c))

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
