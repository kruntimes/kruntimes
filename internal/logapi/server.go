// Package logapi implements the Kubernetes aggregated API used for Run logs.
package logapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/gateway"
)

const (
	Group                  = "logs.kruntimes.io"
	Version                = "v1alpha1"
	defaultTailLines int64 = 100
	maxTailLines     int64 = 500
	defaultTimeout         = 30 * time.Second
	maxTimeout             = 5 * time.Minute
	maxSnapshotBytes int64 = 1 << 20
)

// Server is an aggregated API backend. Kubernetes authenticates callers and
// authorizes the runs/log resource before proxying here. The server accepts
// identity headers only over the aggregation layer's verified mTLS connection,
// then performs an exact SAR for the underlying kruntimes.io Run.
type Server struct {
	Runs                 client.Reader
	Kubernetes           kubernetes.Interface
	PodLogs              gateway.PodLogReader
	Address              string
	TLSCertificateFile   string
	TLSPrivateKeyFile    string
	RequestHeaderCA      []byte
	AllowedClientNames   []string
	UsernameHeaders      []string
	GroupHeaders         []string
	AuthorizationTimeout time.Duration
}

type entry struct {
	gateway.RunLogEntry
	Cursor string `json:"cursor,omitempty"`
}

type cursor struct {
	RunUID string `json:"runUID"`
	Index  int    `json:"index"`
	Hash   string `json:"hash"`
}

func (s *Server) Start(ctx context.Context) error {
	if s.Runs == nil || s.Kubernetes == nil || s.PodLogs == nil {
		return errors.New("Run log API is not configured")
	}
	if s.TLSCertificateFile == "" || s.TLSPrivateKeyFile == "" {
		return errors.New("Run log API requires TLS certificate and private key files")
	}
	if len(s.RequestHeaderCA) == 0 {
		return errors.New("Run log API requires the Kubernetes request-header client CA")
	}
	certificate, err := tls.LoadX509KeyPair(s.TLSCertificateFile, s.TLSPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("load Run log API TLS certificate: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(s.RequestHeaderCA) {
		return errors.New("Kubernetes request-header client CA contains no certificates")
	}
	listener, err := net.Listen("tcp", s.Address)
	if err != nil {
		return fmt.Errorf("listen for Run log API: %w", err)
	}
	server := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate},
			ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs},
	}
	result := make(chan error, 1)
	go func() {
		err := server.ServeTLS(listener, "", "")
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-result:
		return err
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/apis" {
		s.writeJSON(w, http.StatusOK, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{}})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/apis/"+Group {
		s.writeJSON(w, http.StatusOK, map[string]any{"kind": "APIGroup", "apiVersion": "v1", "name": Group, "versions": []map[string]string{{"groupVersion": Group + "/" + Version, "version": Version}}, "preferredVersion": map[string]string{"groupVersion": Group + "/" + Version, "version": Version}})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/apis/"+Group+"/"+Version {
		s.writeJSON(w, http.StatusOK, map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": Group + "/" + Version, "resources": []map[string]any{{"name": "runs/log", "singularName": "", "namespaced": true, "kind": "RunLog", "verbs": []string{"get"}}}})
		return
	}
	namespace, name, ok := logRoute(r.URL.Path)
	if !ok {
		s.writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.serveRunLog(w, r, namespace, name)
}

func logRoute(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 8 || parts[0] != "apis" || parts[1] != Group || parts[2] != Version || parts[3] != "namespaces" || parts[5] != "runs" || parts[7] != "log" || parts[4] == "" || parts[6] == "" {
		return "", "", false
	}
	return parts[4], parts[6], true
}

func (s *Server) serveRunLog(w http.ResponseWriter, r *http.Request, namespace, name string) {
	tail, follow, timeout, requestedCursor, err := parseOptions(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.requestUser(r)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	run := &v1alpha1.Run{}
	if err := s.Runs.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, run); err != nil {
		if apierrors.IsNotFound(err) {
			s.writeError(w, http.StatusNotFound, "Run not found")
		} else {
			s.writeError(w, http.StatusServiceUnavailable, "read Run")
		}
		return
	}
	if err := gateway.AuthorizeRunUser(r.Context(), s.Kubernetes, user, run, s.AuthorizationTimeout); err != nil {
		s.writeAuthorizationError(w, err)
		return
	}
	entries, err := s.snapshot(r.Context(), run)
	if err != nil {
		s.writeReadError(w, err)
		return
	}
	if !follow {
		if int64(len(entries)) > tail {
			entries = entries[len(entries)-int(tail):]
		}
		next := ""
		if len(entries) > 0 {
			next = entries[len(entries)-1].Cursor
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"items": entries, "nextCursor": next})
		return
	}
	s.stream(w, r, run, entries, tail, requestedCursor, timeout)
}

func (s *Server) snapshot(ctx context.Context, run *v1alpha1.Run) ([]entry, error) {
	stream, err := gateway.OpenRunLogStream(ctx, s.PodLogs, run, false, maxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	entries := []entry{}
	index := 0
	err = gateway.ForEachRunLogEntry(stream, string(run.UID), func(value gateway.RunLogEntry) error {
		index++
		entries = append(entries, entry{RunLogEntry: value, Cursor: newCursor(string(run.UID), index, value)})
		return nil
	})
	return entries, err
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run, snapshot []entry, tail int64, requested string, timeout time.Duration) {
	resume, err := decodeCursor(requested)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	last := entry{}
	if resume != nil {
		found := false
		for _, item := range snapshot {
			if item.Cursor == requested {
				found = true
				break
			}
		}
		if !found {
			s.writeError(w, http.StatusGone, "log cursor is no longer retained")
			return
		}
		last.Cursor = requested
	} else if len(snapshot) > 0 {
		start := len(snapshot) - int(tail)
		if start < 0 {
			start = 0
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		flush := http.NewResponseController(w).Flush
		for _, item := range snapshot[start:] {
			if encoder.Encode(item) != nil || flush() != nil {
				return
			}
		}
		last = snapshot[len(snapshot)-1]
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	stream, err := gateway.OpenRunLogStream(ctx, s.PodLogs, run, true, 0)
	if err != nil {
		return
	}
	defer stream.Close()
	encoder, flush := json.NewEncoder(w), http.NewResponseController(w).Flush
	foundLast := last.Cursor == ""
	index := 0
	_ = gateway.ForEachRunLogEntry(stream, string(run.UID), func(value gateway.RunLogEntry) error {
		index++
		item := entry{RunLogEntry: value, Cursor: newCursor(string(run.UID), index, value)}
		if !foundLast {
			if item.Cursor == last.Cursor {
				foundLast = true
			}
			return nil
		}
		if encoder.Encode(item) != nil {
			return io.EOF
		}
		return flush()
	})
}

func newCursor(runUID string, index int, value gateway.RunLogEntry) string {
	payload, _ := json.Marshal(value)
	sum := sha256.Sum256(payload)
	raw, _ := json.Marshal(cursor{RunUID: runUID, Index: index, Hash: base64.RawURLEncoding.EncodeToString(sum[:])})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(value string) (*cursor, error) {
	if value == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	var parsed cursor
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.RunUID == "" || parsed.Index < 1 || parsed.Hash == "" {
		return nil, errors.New("invalid cursor")
	}
	return &parsed, nil
}

func parseOptions(r *http.Request) (int64, bool, time.Duration, string, error) {
	query := r.URL.Query()
	tail := defaultTailLines
	if raw := query.Get("tailLines"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 || value > maxTailLines {
			return 0, false, 0, "", fmt.Errorf("tailLines must be an integer from 1 through %d", maxTailLines)
		}
		tail = value
	}
	follow := query.Get("follow") == "true"
	if raw := query.Get("follow"); raw != "" && raw != "true" && raw != "false" {
		return 0, false, 0, "", errors.New("follow must be true or false")
	}
	timeout := defaultTimeout
	if raw := query.Get("timeoutSeconds"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > maxTimeout {
			return 0, false, 0, "", fmt.Errorf("timeoutSeconds must be an integer from 1 through %d", int(maxTimeout.Seconds()))
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return tail, follow, timeout, query.Get("cursor"), nil
}

func (s *Server) requestUser(r *http.Request) (authenticationv1.UserInfo, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return authenticationv1.UserInfo{}, errors.New("request is not from the Kubernetes aggregation layer")
	}
	if len(s.AllowedClientNames) > 0 {
		valid := false
		for _, name := range s.AllowedClientNames {
			if r.TLS.PeerCertificates[0].Subject.CommonName == name {
				valid = true
				break
			}
		}
		if !valid {
			return authenticationv1.UserInfo{}, errors.New("aggregation client certificate is not allowed")
		}
	}
	username := firstHeader(r, s.UsernameHeaders)
	if username == "" {
		return authenticationv1.UserInfo{}, errors.New("aggregation request has no user identity")
	}
	groups := []string{}
	for _, header := range s.GroupHeaders {
		groups = append(groups, r.Header.Values(header)...)
	}
	return authenticationv1.UserInfo{Username: username, Groups: groups}, nil
}
func firstHeader(r *http.Request, headers []string) string {
	for _, header := range headers {
		if value := r.Header.Get(header); value != "" {
			return value
		}
	}
	return ""
}
func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"error": message})
}
func (s *Server) writeReadError(w http.ResponseWriter, err error) {
	if apierrors.IsNotFound(err) {
		s.writeError(w, http.StatusConflict, "assigned Runtime Pod is no longer available; Run logs cannot be read")
	} else {
		s.writeError(w, http.StatusServiceUnavailable, "read Runtime Pod logs")
	}
}
func (s *Server) writeAuthorizationError(w http.ResponseWriter, err error) {
	message := err.Error()
	code := http.StatusForbidden
	if strings.Contains(message, "not configured") {
		code = http.StatusServiceUnavailable
	}
	s.writeError(w, code, message)
}

// RequestHeaderConfig reads the API server's trusted aggregation identity
// contract from kube-system/extension-apiserver-authentication.
func RequestHeaderConfig(ctx context.Context, kube kubernetes.Interface) ([]byte, []string, []string, []string, error) {
	config, err := kube.CoreV1().ConfigMaps("kube-system").Get(ctx, "extension-apiserver-authentication", metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read extension-apiserver-authentication: %w", err)
	}
	ca := []byte(config.Data["requestheader-client-ca-file"])
	if len(ca) == 0 {
		return nil, nil, nil, nil, errors.New("extension-apiserver-authentication has no requestheader-client-ca-file")
	}
	decode := func(key string, fallback []string) []string {
		var values []string
		if json.Unmarshal([]byte(config.Data[key]), &values) == nil && len(values) > 0 {
			return values
		}
		return fallback
	}
	return ca, decode("requestheader-allowed-names", nil), decode("requestheader-username-headers", []string{"X-Remote-User"}), decode("requestheader-group-headers", []string{"X-Remote-Group"}), nil
}
