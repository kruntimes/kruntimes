package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ConsolePortForward forwards Console Runtime access HTTP requests through one local
// Kubernetes Pod port-forward. It changes only the endpoint scheme and host;
// the Run-owned path, query, and HTTP headers are preserved.
type ConsolePortForward struct {
	httpClient    HTTPDoer
	sessionDialer SessionDialer
	localURL      *url.URL
	stop          chan struct{}
	done          chan struct{}
	err           error
	errMu         sync.RWMutex
	closeOnce     sync.Once
}

// StartConsolePortForward starts a local port-forward to one Ready Pod behind
// the shared Console Service. The returned value implements HTTPDoer
// and can be passed as Config.HTTPClient to NewFromRESTConfig.
func StartConsolePortForward(ctx context.Context, config *rest.Config, namespace, service string, servicePort int) (*ConsolePortForward, error) {
	if config == nil {
		return nil, errors.New("Kubernetes REST config is required")
	}
	if namespace == "" || service == "" {
		return nil, errors.New("Console namespace and service are required")
	}
	if servicePort <= 0 || servicePort > 65535 {
		return nil, fmt.Errorf("invalid Console service port %d", servicePort)
	}
	pods, err := corev1client.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes Pod client: %w", err)
	}
	podName, targetPort, tlsEnabled, err := readyConsoleBackend(ctx, pods, namespace, service, servicePort)
	if err != nil {
		return nil, err
	}
	httpClient, err := consolePortForwardHTTPClient(config, tlsEnabled)
	if err != nil {
		return nil, err
	}
	sessionDialer, err := consolePortForwardSessionDialer(config, tlsEnabled)
	if err != nil {
		return nil, err
	}
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes port-forward transport: %w", err)
	}
	apiURL, err := url.Parse(config.Host)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API URL: %w", err)
	}
	apiURL.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, podName)
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, apiURL)
	stop := make(chan struct{})
	ready := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(targetPort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("create Console port-forward: %w", err)
	}
	forward := &ConsolePortForward{httpClient: httpClient, sessionDialer: sessionDialer, stop: stop, done: make(chan struct{})}
	go func() {
		defer close(forward.done)
		forward.setError(forwarder.ForwardPorts())
	}()
	select {
	case <-ctx.Done():
		forward.Close()
		return nil, ctx.Err()
	case <-ready:
	case <-forward.done:
		return nil, fmt.Errorf("start Console port-forward: %w", forward.Error())
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		forward.Close()
		if err != nil {
			return nil, fmt.Errorf("get Console local port: %w", err)
		}
		return nil, errors.New("Console port-forward did not expose one local port")
	}
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	forward.localURL = &url.URL{Scheme: scheme, Host: "127.0.0.1:" + strconv.Itoa(int(ports[0].Local))}
	return forward, nil
}

func consolePortForwardSessionDialer(config *rest.Config, tlsEnabled bool) (SessionDialer, error) {
	forwardConfig := rest.CopyConfig(config)
	if tlsEnabled {
		forwardConfig.TLSClientConfig.Insecure = true //nolint:gosec // Kubernetes-authenticated local port-forward only.
		forwardConfig.TLSClientConfig.CAData = nil
		forwardConfig.TLSClientConfig.CAFile = ""
	}
	return newRESTSessionDialer(forwardConfig)
}

func consolePortForwardHTTPClient(config *rest.Config, tlsEnabled bool) (*http.Client, error) {
	if !tlsEnabled {
		httpClient, err := rest.HTTPClientFor(config)
		if err != nil {
			return nil, fmt.Errorf("create Console HTTP client: %w", err)
		}
		return httpClient, nil
	}
	// The target is a local Kubernetes port-forward to a Pod selected through
	// the authenticated API. Console certificates name the in-cluster Service,
	// not 127.0.0.1, so normal hostname verification cannot apply here. Keep the
	// remainder of the REST configuration intact: its transport injects the
	// caller's Kubernetes credentials into Console access requests.
	forwardConfig := rest.CopyConfig(config)
	forwardConfig.TLSClientConfig.Insecure = true //nolint:gosec // Kubernetes-authenticated local port-forward only.
	forwardConfig.TLSClientConfig.CAData = nil
	forwardConfig.TLSClientConfig.CAFile = ""
	httpClient, err := rest.HTTPClientFor(forwardConfig)
	if err != nil {
		return nil, fmt.Errorf("create Console HTTPS client: %w", err)
	}
	return httpClient, nil
}

// Do implements HTTPDoer. It fails after the local port-forward exits.
func (f *ConsolePortForward) Do(request *http.Request) (*http.Response, error) {
	if f == nil || f.httpClient == nil || f.localURL == nil {
		return nil, errors.New("Console port-forward is not ready")
	}
	select {
	case <-f.done:
		if err := f.Error(); err != nil {
			return nil, fmt.Errorf("Console port-forward: %w", err)
		}
		return nil, errors.New("Console port-forward is closed")
	default:
	}
	copy := request.Clone(request.Context())
	endpoint := *request.URL
	endpoint.Scheme = f.localURL.Scheme
	endpoint.Host = f.localURL.Host
	copy.URL = &endpoint
	copy.Host = ""
	return f.httpClient.Do(copy)
}

// DialSession implements SessionDialer through the same scoped local
// port-forward used for HTTP gateway requests.
func (f *ConsolePortForward) DialSession(ctx context.Context, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if f == nil || f.sessionDialer == nil || f.localURL == nil {
		return nil, nil, errors.New("Console port-forward is not ready")
	}
	select {
	case <-f.done:
		return nil, nil, errors.New("Console port-forward is closed")
	default:
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("parse Session endpoint: %w", err)
	}
	switch f.localURL.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return nil, nil, errors.New("Console port-forward has an invalid endpoint")
	}
	parsed.Host = f.localURL.Host
	return f.sessionDialer.DialSession(ctx, parsed.String(), headers)
}

// Close stops the local port-forward. It is safe to call more than once.
func (f *ConsolePortForward) Close() {
	if f == nil {
		return
	}
	f.closeOnce.Do(func() { close(f.stop) })
	<-f.done
}

// Error reports a non-nil port-forward failure after it exits.
func (f *ConsolePortForward) Error() error {
	if f == nil {
		return errors.New("Console port-forward is nil")
	}
	f.errMu.RLock()
	defer f.errMu.RUnlock()
	return f.err
}

func (f *ConsolePortForward) setError(err error) {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	f.err = err
}

func readyConsolePod(ctx context.Context, pods corev1client.CoreV1Interface, namespace, service string) (string, error) {
	serviceObject, err := pods.Services(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get Console Service %q: %w", service, err)
	}
	if len(serviceObject.Spec.Selector) == 0 {
		return "", fmt.Errorf("Console Service %q has no selector", service)
	}
	return readyConsolePodForService(ctx, pods, namespace, serviceObject)
}

func readyConsoleBackend(ctx context.Context, pods corev1client.CoreV1Interface, namespace, service string, servicePort int) (string, int, bool, error) {
	serviceObject, err := pods.Services(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return "", 0, false, fmt.Errorf("get Console Service %q: %w", service, err)
	}
	pod, err := readyConsolePodForService(ctx, pods, namespace, serviceObject)
	if err != nil {
		return "", 0, false, err
	}
	servicePortDefinition, err := consoleServicePort(serviceObject, servicePort)
	if err != nil {
		return "", 0, false, err
	}
	targetPort := servicePortDefinition.TargetPort
	if targetPort.Type != intstr.String && targetPort.IntVal == 0 {
		targetPort = intstr.FromInt32(servicePortDefinition.Port)
	}
	if targetPort.Type == intstr.String {
		podObject, err := pods.Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		if err != nil {
			return "", 0, false, fmt.Errorf("get selected Console Pod %q: %w", pod, err)
		}
		port, ok := namedContainerPort(podObject, targetPort.StrVal)
		if !ok {
			return "", 0, false, fmt.Errorf("Console Service %q targetPort %q is not declared by Pod %q", service, targetPort.StrVal, pod)
		}
		return pod, port, consoleServicePortUsesTLS(servicePortDefinition), nil
	}
	if targetPort.IntVal <= 0 || targetPort.IntVal > 65535 {
		return "", 0, false, fmt.Errorf("Console Service %q has invalid targetPort %d", service, targetPort.IntVal)
	}
	return pod, int(targetPort.IntVal), consoleServicePortUsesTLS(servicePortDefinition), nil
}

func readyConsolePodForService(ctx context.Context, pods corev1client.CoreV1Interface, namespace string, service *corev1.Service) (string, error) {
	if service == nil {
		return "", errors.New("Console Service is required")
	}
	if len(service.Spec.Selector) == 0 {
		return "", fmt.Errorf("Console Service %q has no selector", service.Name)
	}
	list, err := pods.Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set(service.Spec.Selector).String()})
	if err != nil {
		return "", fmt.Errorf("list Console Pods: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		if podReady(&list.Items[i]) {
			names = append(names, list.Items[i].Name)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("Console Service %q has no Ready Pods", service.Name)
	}
	sort.Strings(names)
	return names[0], nil
}

func consoleServicePort(service *corev1.Service, requestedPort int) (corev1.ServicePort, error) {
	if service == nil {
		return corev1.ServicePort{}, errors.New("Console Service is required")
	}
	for _, port := range service.Spec.Ports {
		if int(port.Port) != requestedPort {
			continue
		}
		return port, nil
	}
	return corev1.ServicePort{}, fmt.Errorf("Console Service %q does not expose port %d", service.Name, requestedPort)
}

func consoleServicePortUsesTLS(port corev1.ServicePort) bool {
	return strings.EqualFold(port.Name, "https") || port.Port == 443
}

func namedContainerPort(pod *corev1.Pod, name string) (int, bool) {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == name && port.ContainerPort > 0 {
				return int(port.ContainerPort), true
			}
		}
	}
	return 0, false
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
