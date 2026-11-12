package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultDataDir = "plugins/cliproxy-tracing"
)

// config is the validated plugin configuration under plugins.configs.cliproxy-tracing.
//
// Content capture is opt-in. The plugin exists to export traces, and content is
// the reason it exists, but an install that has not asked for prompts to leave
// the machine should not have them leave. CaptureContent therefore defaults to
// false and every content path checks it.
type config struct {
	Enabled     bool
	Endpoint    string
	Headers     map[string]string
	ServiceName string

	// CaptureContent turns on prompt and completion export. Off by default.
	CaptureContent bool
	// MaxContentChars caps one message's content attribute.
	MaxContentChars int
	// MaxTraceContentBytes caps the whole content of one trace, prompt and
	// completion each. A conversation with many turns would otherwise build an
	// export larger than the backend accepts, and a backend rejects the whole
	// request, not just the oversized span.
	MaxTraceContentBytes int
	// RequireComplete drops requests whose completion did not finish, so an
	// export is usable for analysis rather than full of half-answers.
	RequireComplete bool

	SampleRate float64

	ExportTimeout time.Duration
	FlushInterval time.Duration
	MaxBatchSpans int
	// MaxBatchBytes caps one OTLP export request. Backends enforce a hard body
	// limit (PostHog: 4 MiB) and answer 413 above it, discarding every span in
	// the batch, so this is kept under that with headroom.
	MaxBatchBytes int

	// CompletionGrace is how long a finished request waits for late usage
	// records before its trace is closed without them.
	CompletionGrace time.Duration
}

func defaultConfig() config {
	return config{
		Enabled:              true,
		Headers:              map[string]string{},
		ServiceName:          "cliproxyapi",
		CaptureContent:       false,
		MaxContentChars:      32 * 1024,
		MaxTraceContentBytes: 512 * 1024,
		RequireComplete:      true,
		SampleRate:           1,
		ExportTimeout:        10 * time.Second,
		FlushInterval:        time.Second,
		MaxBatchSpans:        512,
		MaxBatchBytes:        defaultExportBudget,
		CompletionGrace:      2 * time.Second,
	}
}

func (c config) endpointSet() bool {
	return strings.TrimSpace(c.Endpoint) != ""
}

// active reports whether the exporter should run at all.
func (c config) active() bool { return c.Enabled && c.endpointSet() }

func parseConfig(raw []byte) (config, []string, error) {
	cfg := defaultConfig()
	var warnings []string
	values := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &values); err != nil {
			return cfg, nil, fmt.Errorf("cliproxy-tracing: invalid config yaml: %w", err)
		}
	}

	if v, ok := values["enabled"]; ok && v != nil {
		if b, okB := toBool(v); okB {
			cfg.Enabled = b
		}
	}
	if v, ok := values["endpoint"]; ok && v != nil {
		endpoint := strings.TrimSpace(fmt.Sprint(v))
		if endpoint != "" {
			if err := validateEndpoint(endpoint); err != nil {
				warnings = append(warnings, fmt.Sprintf("endpoint: %v; export disabled", err))
			} else {
				cfg.Endpoint = endpoint
			}
		}
	}
	if v, ok := values["service_name"]; ok && v != nil {
		if name := strings.TrimSpace(fmt.Sprint(v)); name != "" {
			cfg.ServiceName = name
		}
	}
	if v, ok := values["capture_content"]; ok && v != nil {
		if b, okB := toBool(v); okB {
			cfg.CaptureContent = b
		} else {
			warnings = append(warnings, "capture_content: expected a boolean")
		}
	}
	if v, ok := values["require_complete"]; ok && v != nil {
		if b, okB := toBool(v); okB {
			cfg.RequireComplete = b
		} else {
			warnings = append(warnings, "require_complete: expected a boolean")
		}
	}
	intField(values, "max_content_chars", &cfg.MaxContentChars, 256, 1<<20, &warnings)
	intField(values, "max_trace_content_bytes", &cfg.MaxTraceContentBytes, 4096, 4<<20, &warnings)
	floatField(values, "sample_rate", &cfg.SampleRate, 0, 1, &warnings)
	durationField(values, "export_timeout_ms", &cfg.ExportTimeout, 100*time.Millisecond, maxExportTimeout, &warnings)
	durationField(values, "flush_interval_ms", &cfg.FlushInterval, 50*time.Millisecond, time.Minute, &warnings)
	durationField(values, "completion_grace_ms", &cfg.CompletionGrace, 0, 5*time.Minute, &warnings)
	intField(values, "max_batch_spans", &cfg.MaxBatchSpans, 1, 10000, &warnings)
	intField(values, "max_batch_bytes", &cfg.MaxBatchBytes, 64<<10, maxExportBytes, &warnings)

	headers, headerWarnings := parseHeaders(values["headers"])
	cfg.Headers = headers
	warnings = append(warnings, headerWarnings...)

	if cfg.MaxBatchSpans > maxQueuedSpans {
		cfg.MaxBatchSpans = maxQueuedSpans
	}
	return cfg, warnings, nil
}

func validateEndpoint(endpoint string) error {
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return fmt.Errorf("scheme must be http or https")
	}
	return nil
}

// parseHeaders accepts either a mapping or a list of "Name: Value" strings.
// Values are never logged or echoed in warnings.
func parseHeaders(v any) (map[string]string, []string) {
	out := map[string]string{}
	var warnings []string

	add := func(name, value string) {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" {
			return
		}
		// Header values must not contain newlines: this is the one place a
		// config value reaches a network request directly.
		if strings.ContainsAny(value, "\r\n\x00") {
			warnings = append(warnings, fmt.Sprintf("headers: value of %q contains control characters; ignored", name))
			return
		}
		out[name] = value
	}

	switch typed := v.(type) {
	case nil:
	case map[string]any:
		for name, value := range typed {
			add(name, fmt.Sprint(value))
		}
	case []any:
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				name, value, ok := strings.Cut(entry, ":")
				if !ok {
					warnings = append(warnings, "headers: list entry without ':' ignored")
					continue
				}
				add(name, value)
			case map[string]any:
				name := fmt.Sprint(entry["name"])
				if name == "" || name == "<nil>" {
					name = fmt.Sprint(entry["key"])
				}
				add(name, fmt.Sprint(entry["value"]))
			}
		}
	default:
		warnings = append(warnings, "headers: expected a mapping or a list; ignored")
	}
	return out, warnings
}

func toBool(v any) (bool, bool) {
	switch typed := v.(type) {
	case bool:
		return typed, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(typed))
		return b, err == nil
	}
	return false, false
}

func intField(values map[string]any, key string, dst *int, lo, hi int, warnings *[]string) {
	v, ok := values[key]
	if !ok || v == nil {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("%s: expected an integer", key))
		return
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	*dst = n
}

func floatField(values map[string]any, key string, dst *float64, lo, hi float64, warnings *[]string) {
	v, ok := values[key]
	if !ok || v == nil {
		return
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("%s: expected a number", key))
		return
	}
	if f < lo {
		f = lo
	}
	if f > hi {
		f = hi
	}
	*dst = f
}

func durationField(values map[string]any, key string, dst *time.Duration, lo, hi time.Duration, warnings *[]string) {
	v, ok := values[key]
	if !ok || v == nil {
		return
	}
	ms, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("%s: expected an integer number of milliseconds", key))
		return
	}
	d := time.Duration(ms) * time.Millisecond
	if d < lo {
		d = lo
	}
	if d > hi {
		d = hi
	}
	*dst = d
}

// configFields is the schema the management client renders.
func configFields() []configField {
	d := defaultConfig()
	return []configField{
		{"endpoint", "string", nil, "OTLP/HTTP traces URL. PostHog: https://us.i.posthog.com/i/v0/ai/otel. Langfuse: https://cloud.langfuse.com/api/public/otel. Empty disables export."},
		{"headers", "object", nil, "Extra export headers, e.g. Authorization. Values are never logged."},
		{"service_name", "string", d.ServiceName, "OpenTelemetry service.name resource attribute."},
		{"capture_content", "boolean", d.CaptureContent, "Export prompt and completion text. Off by default."},
		{"max_content_chars", "integer", d.MaxContentChars, "Longest single message content exported, in characters."},
		{"max_trace_content_bytes", "integer", d.MaxTraceContentBytes, "Largest prompt or completion exported for one trace, in bytes. Longer conversations are truncated from the middle of the oldest messages."},
		{"require_complete", "boolean", d.RequireComplete, "Skip requests whose completion did not finish, so exports contain complete exchanges only."},
		{"sample_rate", "number", d.SampleRate, "Fraction of traces to export (0..1)."},
		{"export_timeout_ms", "integer", d.ExportTimeout.Milliseconds(), "HTTP timeout for one export attempt."},
		{"flush_interval_ms", "integer", d.FlushInterval.Milliseconds(), "How often queued spans are exported."},
		{"max_batch_spans", "integer", d.MaxBatchSpans, "Spans per export request."},
		{"max_batch_bytes", "integer", d.MaxBatchBytes, "Encoded size cap for one export request. Backends reject an oversized body and discard every span in it, so batches are split under this."},
		{"completion_grace_ms", "integer", d.CompletionGrace.Milliseconds(), "How long a finished request waits for late usage records."},
	}
}
