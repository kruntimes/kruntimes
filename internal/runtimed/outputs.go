package runtimed

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kruntimes/kruntimes/internal/artifact"
)

const (
	reasonOutputsInvalid  = "OutputsInvalid"
	reasonOutputsTooLarge = "OutputsTooLarge"
)

type outputsTooLargeError struct {
	message string
}

func (e *outputsTooLargeError) Error() string {
	return e.message
}

func isOutputsTooLarge(err error) bool {
	var target *outputsTooLargeError
	return errors.As(err, &target)
}

func outputsPath(workingDir string) string {
	return filepath.Join(workingDir, "outputs")
}

// readOutputs parses KEY=VALUE lines. Duplicate keys are deterministic: the
// last declaration replaces the previous value.
func readOutputs(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read outputs: %w", err)
	}
	defer f.Close()

	outputs := make(map[string]string)
	totalBytes := 0
	reader := bufio.NewReader(f)
	lineNumber := 0
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read outputs: %w", readErr)
		}
		if line != "" {
			lineNumber++
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if !utf8.ValidString(line) {
				return nil, fmt.Errorf("outputs line %d is not valid UTF-8", lineNumber)
			}
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				key, value, ok := strings.Cut(trimmed, "=")
				if !ok {
					return nil, fmt.Errorf("outputs line %d must use KEY=VALUE format", lineNumber)
				}
				key = strings.TrimSpace(key)
				value = strings.TrimSpace(value)
				if key == "" {
					return nil, fmt.Errorf("outputs line %d has an empty key", lineNumber)
				}
				if len(key) > artifact.MaxOutputKeyBytes {
					return nil, &outputsTooLargeError{message: fmt.Sprintf(
						"output key on line %d exceeds %d bytes", lineNumber, artifact.MaxOutputKeyBytes,
					)}
				}
				if len(value) > artifact.MaxOutputValueBytes {
					return nil, &outputsTooLargeError{message: fmt.Sprintf(
						"output value for %q exceeds %d bytes", key, artifact.MaxOutputValueBytes,
					)}
				}

				if previous, exists := outputs[key]; exists {
					totalBytes -= len(key) + len(previous)
				} else if len(outputs) >= artifact.MaxOutputKeys {
					return nil, &outputsTooLargeError{message: fmt.Sprintf(
						"outputs exceed %d keys", artifact.MaxOutputKeys,
					)}
				}
				totalBytes += len(key) + len(value)
				if totalBytes > artifact.MaxOutputsBytes {
					return nil, &outputsTooLargeError{message: fmt.Sprintf(
						"outputs exceed %d bytes", artifact.MaxOutputsBytes,
					)}
				}
				outputs[key] = value
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}
