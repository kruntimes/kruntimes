package krt

import (
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestExplicitBearerConfigPrefersCommandLineToken(t *testing.T) {
	token := "command-line-token"
	flags := genericclioptions.NewConfigFlags(false)
	flags.BearerToken = &token

	config := &rest.Config{Host: "https://kubernetes.example", BearerToken: "kubeconfig-token", BearerTokenFile: "/token", ExecProvider: &clientcmdapi.ExecConfig{Command: "credential-plugin"}}
	loaded := explicitBearerConfig(config, flags)
	if loaded.BearerToken != token || loaded.BearerTokenFile != "" || loaded.ExecProvider != nil {
		t.Fatalf("rest config = %#v, want explicit bearer-only credentials", loaded)
	}
}
