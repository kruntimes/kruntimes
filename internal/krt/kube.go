package krt

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func clientFromConfig(getter genericclioptions.RESTClientGetter, scheme *runtime.Scheme) (client.Client, error) {
	restConfig, err := getter.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return client.New(restConfig, client.Options{Scheme: scheme})
}

func restConfigFromConfig(getter genericclioptions.RESTClientGetter) (*rest.Config, error) {
	loaded, err := getter.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return loaded, nil
}

func namespaceFromConfig(getter genericclioptions.RESTClientGetter) string {
	namespace, _, err := getter.ToRawKubeConfigLoader().Namespace()
	if err == nil && namespace != "" {
		return namespace
	}
	return "default"
}
