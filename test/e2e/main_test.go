package e2e

// Shared E2E test bootstrap: Kubernetes clients, image references, and the enabled-feature switches.
//
// This file owns process-wide setup only. Runtime fixtures, condition waits,
// gateway helpers, and the scenario suites live in sibling files so each
// responsibility can evolve independently.

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	certManagerE2EEnabledEnv      = "KRUNTIMES_E2E_CERT_MANAGER"
	certManagerGatewayCertificate = "kruntimes-console"
	certManagerGatewayTLSSecret   = "kruntimes-console-cert-manager-tls"
)

const testNamespace = "default"

var k8sClient client.Client

var restConfig *rest.Config

var coreClientset *kubernetes.Clientset

func bashRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_BASH_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-bash-runtime:latest"
}

func pythonRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_PYTHON_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-python-runtime:latest"
}

func diagnosisRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_DIAGNOSIS_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-diagnosis-runtime:latest"
}

func runtimedImage() string {
	if image := os.Getenv("KRUNTIMES_RUNTIMED_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-runtimed:latest"
}

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	restConfig = config.GetConfigOrDie()
	restConfig.QPS = 50
	restConfig.Burst = 100

	var err error
	k8sClient, err = client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		os.Exit(1)
	}
	coreClientset, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		os.Exit(1)
	}

	os.Exit(m.Run())
}
