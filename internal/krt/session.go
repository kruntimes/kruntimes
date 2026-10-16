package krt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"

	"github.com/kruntimes/kruntimes/sdk/go/sandbox"
)

const (
	sessionOutputText   = "text"
	sessionOutputNDJSON = "ndjson"
)

func newSessionCmd(getter genericclioptions.RESTClientGetter) *cobra.Command {
	command := &cobra.Command{Use: "session", Short: "Operate an existing ready Session Run."}
	command.AddCommand(newSessionExecCmd(getter), newSessionAttachCmd(getter))
	return command
}

func newSessionExecCmd(getter genericclioptions.RESTClientGetter) *cobra.Command {
	var output string
	var showEvents bool
	command := &cobra.Command{
		Use:          "exec <run> -- <command> [args...]",
		Short:        "Execute one command in a ready Session Run.",
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSessionOutput(output); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			session, closeSession, err := openKRTSession(ctx, getter, args[0])
			if err != nil {
				return err
			}
			defer closeSession()
			return runSessionOperation(ctx, session, sandbox.Operation{Command: &sandbox.Command{Argv: args[1:]}}, output, showEvents, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	command.Flags().StringVarP(&output, "output", "o", sessionOutputText, "Event output: text or ndjson")
	command.Flags().BoolVar(&showEvents, "show-events", false, "Show Session operation IDs, progress, and completion messages in text output")
	return command
}

func newSessionAttachCmd(getter genericclioptions.RESTClientGetter) *cobra.Command {
	var output, input string
	var showEvents bool
	command := &cobra.Command{
		Use:          "attach <run>",
		Short:        "Submit commands to an existing Session Run, one line at a time.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSessionOutput(output); err != nil {
				return err
			}
			if input != "command" && input != sessionOutputNDJSON {
				return fmt.Errorf("unsupported Session input %q: use command or ndjson", input)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			session, closeSession, err := openKRTSession(ctx, getter, args[0])
			if err != nil {
				return err
			}
			defer closeSession()
			return attachSession(ctx, session, args[0], cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), input, output, showEvents)
		},
	}
	command.Flags().StringVarP(&output, "output", "o", sessionOutputText, "Event output: text or ndjson")
	command.Flags().StringVar(&input, "input", "command", "Input lines: command text or ndjson operations")
	command.Flags().BoolVar(&showEvents, "show-events", false, "Show Session operation IDs, progress, and completion messages in text output")
	return command
}

func validateSessionOutput(output string) error {
	if output != sessionOutputText && output != sessionOutputNDJSON {
		return fmt.Errorf("unsupported Session output %q: use text or ndjson", output)
	}
	return nil
}

func openKRTSession(ctx context.Context, getter genericclioptions.RESTClientGetter, runName string) (*sandbox.Session, func(), error) {
	config, err := restConfigFromConfig(getter)
	if err != nil {
		return nil, nil, err
	}
	client, err := sandbox.NewFromRESTConfig(config, sandbox.Config{})
	if err != nil {
		return nil, nil, fmt.Errorf("configure Session client: %w", err)
	}
	opened, err := client.Open(ctx, namespaceFromConfig(getter), runName)
	if err != nil {
		return nil, nil, err
	}
	session, err := opened.OpenSession(ctx)
	if err == nil {
		return session, func() { _ = session.Close() }, nil
	}
	directErr := err
	endpoint := opened.Run().Status.Endpoint
	if endpoint == nil {
		return nil, nil, explainSessionUnauthorized(directErr, config)
	}
	consoleNamespace, consoleService, consolePort, ok := consoleServiceFromEndpoint(endpoint.URL)
	if !ok || ctx.Err() != nil {
		return nil, nil, explainSessionUnauthorized(directErr, config)
	}
	forward, err := sandbox.StartConsolePortForward(ctx, config, consoleNamespace, consoleService, consolePort)
	if err != nil {
		return nil, nil, fmt.Errorf("open Session directly: %v; start Console port-forward: %w", directErr, err)
	}
	forwardClient, err := sandbox.NewFromRESTConfig(config, sandbox.Config{SessionDialer: forward})
	if err == nil {
		opened, err = forwardClient.Open(ctx, namespaceFromConfig(getter), runName)
	}
	if err == nil {
		session, err = opened.OpenSession(ctx)
	}
	if err != nil {
		forward.Close()
		return nil, nil, fmt.Errorf("open Session through Console port-forward: %w", explainSessionUnauthorized(err, config))
	}
	return session, func() { _ = session.Close(); forward.Close() }, nil
}

func explainSessionUnauthorized(err error, config *rest.Config) error {
	var apiError *sandbox.APIError
	if config == nil || !errors.As(err, &apiError) || apiError.StatusCode != http.StatusUnauthorized {
		return err
	}
	if config.BearerToken != "" || config.BearerTokenFile != "" || config.ExecProvider != nil || config.AuthProvider != nil ||
		(len(config.CertData) == 0 && config.CertFile == "") {
		return err
	}
	return fmt.Errorf("%w: Console did not authenticate the kubeconfig client certificate; configure console.tls.clientCASecretName with its signing CA, or use --token with a bearer token authorized to get this Run", err)
}

func consoleServiceFromEndpoint(raw string) (namespace, service string, port int, ok bool) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Hostname() == "" {
		return "", "", 0, false
	}
	parts := strings.Split(endpoint.Hostname(), ".")
	if len(parts) < 3 || parts[2] != "svc" || parts[0] == "" || parts[1] == "" {
		return "", "", 0, false
	}
	port = 443
	if endpoint.Scheme == "http" {
		port = 80
	}
	if endpoint.Port() != "" {
		port, err = strconv.Atoi(endpoint.Port())
		if err != nil || port <= 0 || port > 65535 {
			return "", "", 0, false
		}
	}
	return parts[1], parts[0], port, true
}

func attachSession(ctx context.Context, session *sandbox.Session, runName string, input io.Reader, stdout, stderr io.Writer, inputFormat, outputFormat string, showEvents bool) error {
	type scannedLine struct {
		value string
		err   error
	}
	lines := make(chan scannedLine)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		for scanner.Scan() {
			select {
			case lines <- scannedLine{value: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- scannedLine{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	interactive := false
	if file, ok := input.(*os.File); ok {
		interactive = term.IsTerminal(int(file.Fd()))
	}
	var lastFailure error
	for {
		if interactive {
			if _, err := fmt.Fprintf(stderr, "%s> ", runName); err != nil {
				return err
			}
		}
		var line scannedLine
		select {
		case <-ctx.Done():
			return fmt.Errorf("session interrupted: %w", ctx.Err())
		case next, ok := <-lines:
			if !ok {
				return lastFailure
			}
			line = next
		}
		if line.err != nil {
			return fmt.Errorf("read Session input: %w", line.err)
		}
		if strings.TrimSpace(line.value) == "" {
			continue
		}
		var operation sandbox.Operation
		if inputFormat == sessionOutputNDJSON {
			if err := json.Unmarshal([]byte(line.value), &operation); err != nil {
				return fmt.Errorf("decode Session operation: %w", err)
			}
		} else {
			operation.Command = &sandbox.Command{Shell: line.value}
		}
		if err := runSessionOperation(ctx, session, operation, outputFormat, showEvents, stdout, stderr); err != nil {
			if ctx.Err() != nil || !isSessionOperationFailure(err) {
				return err
			}
			lastFailure = err
			if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
				return writeErr
			}
		}
	}
}

type sessionOperationFailure struct{ message string }

func (e sessionOperationFailure) Error() string { return e.message }

func isSessionOperationFailure(err error) bool {
	var failure sessionOperationFailure
	return errors.As(err, &failure)
}

func runSessionOperation(ctx context.Context, session *sandbox.Session, operation sandbox.Operation, format string, showEvents bool, stdout, stderr io.Writer) error {
	operationID, err := session.Send(ctx, operation)
	if err != nil {
		return fmt.Errorf("submit Session operation: %w", err)
	}
	// Send consumes the first accepted frame and returns its operation ID.
	accepted := sandbox.OperationEvent{Sequence: 1, Type: "accepted"}
	accepted.Accepted = &struct {
		OperationID string `json:"operationID"`
	}{OperationID: operationID}
	if err := renderSessionEvent(accepted, format, showEvents, stdout, stderr); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := session.Cancel(cancelCtx, operationID)
			cancel()
			if err != nil {
				_ = session.Close()
				return
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = session.Close()
			}
		case <-done:
		}
	}()
	for {
		// Keep the read active long enough to receive the server's cancellation
		// response after an interrupt; the signal handler sends cancel separately.
		readCtx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		event, err := session.Receive(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("receive Session operation: %w", err)
		}
		if err := renderSessionEvent(event, format, showEvents, stdout, stderr); err != nil {
			return err
		}
		if event.Failed != nil {
			return sessionOperationFailure{message: fmt.Sprintf("Session operation failed (%d): %s", event.Failed.Code, event.Failed.Message)}
		}
		if event.Completed != nil {
			if command := event.Completed.Command; command != nil {
				if command.TimedOut {
					return sessionOperationFailure{message: "Session command timed out"}
				}
				if command.ExitCode != 0 {
					return sessionOperationFailure{message: fmt.Sprintf("Session command exited with status %d", command.ExitCode)}
				}
			}
			return nil
		}
	}
}

func renderSessionEvent(event sandbox.OperationEvent, format string, showEvents bool, stdout, stderr io.Writer) error {
	if format == sessionOutputNDJSON {
		return json.NewEncoder(stdout).Encode(event)
	}
	switch event.Type {
	case "accepted":
		if !showEvents {
			return nil
		}
		_, err := fmt.Fprintf(stderr, "Session operation accepted: %s\n", event.Accepted.OperationID)
		return err
	case "output":
		if event.Output == nil {
			return errors.New("session output event has no payload")
		}
		target := stdout
		if event.Output.Stream == "stderr" {
			target = stderr
		}
		_, err := target.Write(event.Output.Data)
		return err
	case "progress":
		if event.Progress == nil {
			return errors.New("session progress event has no payload")
		}
		if event.Progress.Kind == "text_delta" {
			_, err := stdout.Write(event.Progress.Data)
			return err
		}
		if !showEvents {
			return nil
		}
		_, err := fmt.Fprintf(stderr, "Session progress (%s): %s\n", event.Progress.Kind, event.Progress.Message)
		return err
	case "completed":
		if !showEvents {
			return nil
		}
		_, err := fmt.Fprintln(stderr, "Session operation completed")
		return err
	case "failed":
		return nil
	default:
		return fmt.Errorf("unsupported Session event %q", event.Type)
	}
}
