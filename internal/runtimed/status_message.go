package runtimed

import (
	"strings"
	"unicode/utf8"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

const maxStatusMessageBytes = 1024

func summarizeRuntimeFailure(resp *pb.StatusResponse) string {
	if resp == nil {
		return "runtime execution failed"
	}
	if resp.ErrorMessage != "" {
		return boundedStatusMessage(resp.ErrorMessage)
	}
	if resp.Stderr != "" {
		return boundedStatusMessageTail("runtime stderr: ", resp.Stderr)
	}
	return "runtime execution failed"
}

func boundedStatusMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= maxStatusMessageBytes {
		return message
	}
	const suffix = "..."
	return truncateUTF8Prefix(message, maxStatusMessageBytes-len(suffix)) + suffix
}

func boundedStatusMessageTail(prefix, message string) string {
	message = strings.ToValidUTF8(strings.TrimSpace(message), "\uFFFD")
	if len(prefix)+len(message) <= maxStatusMessageBytes {
		return prefix + message
	}
	const marker = "... "
	available := maxStatusMessageBytes - len(prefix) - len(marker)
	return prefix + marker + truncateUTF8Tail(message, available)
}

func truncateUTF8Prefix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func truncateUTF8Tail(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	start := len(value) - maxBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}
