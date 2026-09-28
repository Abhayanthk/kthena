/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
)

// ParseRouterAccessLogLine parses either supported Router access-log format.
func ParseRouterAccessLogLine(line []byte) (accesslog.AccessLogEntry, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return accesslog.AccessLogEntry{}, false
	}
	if line[0] == '{' {
		var entry accesslog.AccessLogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return accesslog.AccessLogEntry{}, false
		}
		return entry, isRouterAccessLogEntry(entry)
	}
	return parseTextRouterAccessLog(string(line))
}

func parseTextRouterAccessLog(line string) (accesslog.AccessLogEntry, bool) {
	closingBracket := strings.IndexByte(line, ']')
	firstQuote := strings.IndexByte(line, '"')
	if closingBracket < 1 || firstQuote < closingBracket {
		return accesslog.AccessLogEntry{}, false
	}
	secondQuoteOffset := strings.IndexByte(line[firstQuote+1:], '"')
	if secondQuoteOffset < 0 {
		return accesslog.AccessLogEntry{}, false
	}
	secondQuote := firstQuote + 1 + secondQuoteOffset

	requestFields := strings.Fields(line[firstQuote+1 : secondQuote])
	remainder := strings.Fields(line[secondQuote+1:])
	if len(requestFields) != 3 || len(remainder) == 0 {
		return accesslog.AccessLogEntry{}, false
	}
	statusCode, err := strconv.Atoi(remainder[0])
	if err != nil {
		return accesslog.AccessLogEntry{}, false
	}
	entry := accesslog.AccessLogEntry{
		Method:     requestFields[0],
		Path:       requestFields[1],
		Protocol:   requestFields[2],
		StatusCode: statusCode,
	}
	entry.Timestamp, _ = time.Parse(time.RFC3339Nano, line[1:closingBracket])

	// String values may be quoted, and a quoted one can hold spaces, so read them
	// by field name rather than by splitting the line on whitespace.
	for name, target := range map[string]*string{
		"model_name":     &entry.ModelName,
		"model_route":    &entry.ModelRoute,
		"model_server":   &entry.ModelServer,
		"selected_pod":   &entry.SelectedPod,
		"request_id":     &entry.RequestID,
		"backend_type":   &entry.BackendType,
		"backend_name":   &entry.BackendName,
		"upstream_model": &entry.UpstreamModel,
		"error_origin":   &entry.ErrorOrigin,
		"gateway":        &entry.Gateway,
		"http_route":     &entry.HTTPRoute,
		"inference_pool": &entry.InferencePool,
	} {
		if value, ok := textAccessLogField(line, name); ok {
			*target = unquoteTextAccessLogValue(value)
		}
	}

	for _, field := range remainder[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "upstream_status_code":
			entry.UpstreamStatusCode, _ = strconv.Atoi(value)
		case "upstream_attempts":
			entry.UpstreamAttempts, _ = strconv.Atoi(value)
		case "tokens":
			_, _ = fmt.Sscanf(value, "%d/%d", &entry.InputTokens, &entry.OutputTokens)
		case "timings":
			_, _ = fmt.Sscanf(value, "%dms(%d+%d+%d)",
				&entry.DurationTotal,
				&entry.DurationRequestProcessing,
				&entry.DurationUpstreamProcessing,
				&entry.DurationResponseProcessing,
			)
		}
	}
	if value, ok := textAccessLogField(line, "error"); ok {
		errorType, message, _ := strings.Cut(unquoteTextAccessLogValue(value), ":")
		entry.Error = &accesslog.ErrorInfo{Type: errorType, Message: message}
	}
	return entry, isRouterAccessLogEntry(entry)
}

var textAccessLogFieldNames = []string{
	"error",
	"model_name",
	"model_route",
	"model_server",
	"selected_pod",
	"request_id",
	"backend_type",
	"backend_name",
	"upstream_model",
	"upstream_status_code",
	"upstream_attempts",
	"error_origin",
	"gateway",
	"http_route",
	"inference_pool",
	"tokens",
	"timings",
}

func unquoteTextAccessLogValue(value string) string {
	if !strings.HasPrefix(value, `"`) {
		return value
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}

func textAccessLogField(line, name string) (string, bool) {
	marker := " " + name + "="
	start := strings.Index(line, marker)
	if start < 0 {
		return "", false
	}
	value := line[start+len(marker):]
	end := len(value)
	for _, nextName := range textAccessLogFieldNames {
		next := strings.Index(value, " "+nextName+"=")
		if next >= 0 && next < end {
			end = next
		}
	}
	return value[:end], true
}

func isRouterAccessLogEntry(entry accesslog.AccessLogEntry) bool {
	return entry.Method != "" && entry.Path != "" && entry.Protocol != "" && entry.StatusCode > 0
}
