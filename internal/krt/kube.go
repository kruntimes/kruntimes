package krt

import (
	"fmt"
	"strings"

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
	// ConfigFlags normally applies --token while loading the kubeconfig. Make
	// that override explicit as well: an exec plugin or client certificate from
	// the selected kubeconfig user must never supersede a token supplied on the
	// command line.
	if flags, ok := getter.(*genericclioptions.ConfigFlags); ok {
		return explicitBearerConfig(loaded, flags), nil
	}
	return loaded, nil
}

func explicitBearerConfig(loaded *rest.Config, flags *genericclioptions.ConfigFlags) *rest.Config {
	if flags == nil || flags.BearerToken == nil || strings.TrimSpace(*flags.BearerToken) == "" {
		return loaded
	}
	result := rest.AnonymousClientConfig(loaded)
	result.BearerToken = strings.TrimSpace(*flags.BearerToken)
	result.BearerTokenFile = ""
	return result
}

func namespaceFromConfig(getter genericclioptions.RESTClientGetter) string {
	namespace, _, err := getter.ToRawKubeConfigLoader().Namespace()
	if err == nil && namespace != "" {
		return namespace
	}
	return "default"
}
