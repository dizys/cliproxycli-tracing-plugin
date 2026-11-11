package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func resetPlugin(t *testing.T) {
	t.Helper()
	plugin = newPluginInstance()
}

func testConfig() config {
	cfg := defaultConfig()
	cfg.CaptureContent = true
	cfg.RequireComplete = true
	return cfg
}

// The whole point of this plugin over the one it replaces: a span must carry
// gen_ai.operation.name so PostHog classifies it as a generation.
func TestSpansCarryGenerationMarker(t *testing.T) {
	tr := &finishedTrace{
		TraceID: "trace-1", RequestID: "req-1", Outcome: "succeeded",
		StartedAt: base, CompletedAt: base.Add(time.Second),
		PromptJSON: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		Completion: []byte("hi there"),
		Attempts: []usageRecord{{
			Provider: "claude", Model: "claude-opus-5-5",
			RequestedAt: base, Latency: time.Second,
			Detail: usageDetail{InputTokens: 10, OutputTokens: 3},
		}},
	}
	spans := buildTraceSpans(tr, testConfig(), base)

	var attempt *otlpSpan
	for i := range spans {
		if spans[i].Kind == spanKindClient {
			attempt = &spans[i]
		}
	}
	if attempt == nil {
		t.Fatal("no attempt span built")
	}
	keys := attrKeys(attempt.Attributes)
	if keys["gen_ai.operation.name"] != "chat" {
		t.Fatalf("gen_ai.operation.name = %q, want chat", keys["gen_ai.operation.name"])
	}
	if keys["gen_ai.provider.name"] != "claude" {
		t.Fatalf("gen_ai.provider.name = %q", keys["gen_ai.provider.name"])
	}
}

// Both backends read content from different attributes, so one span has to
// carry all of them.
func TestSpansCarryContentForBothBackends(t *testing.T) {
	tr := &finishedTrace{
		TraceID: "trace-2", RequestID: "req-2", Outcome: "succeeded",
		StartedAt: base, CompletedAt: base.Add(time.Second),
		PromptJSON: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		Completion: []byte("hi there"),
		Attempts:   []usageRecord{{Provider: "claude", Model: "m", RequestedAt: base, Latency: time.Second}},
	}
	spans := buildTraceSpans(tr, testConfig(), base)
	keys := attrKeys(spans[1].Attributes)

	for _, want := range []string{
		"gen_ai.input.messages",       // PostHog -> $ai_input
		"gen_ai.output.messages",      // PostHog -> $ai_output_choices
		"langfuse.observation.input",  // Langfuse native
		"langfuse.observation.output", // Langfuse native
		"gen_ai.prompt",               // Langfuse generic convention
		"gen_ai.completion",
	} {
		if keys[want] == "" {
			t.Fatalf("missing %q", want)
		}
	}
	if !strings.Contains(keys["gen_ai.input.messages"], "hello") {
		t.Fatalf("input messages lost the prompt: %q", keys["gen_ai.input.messages"])
	}
	if !strings.Contains(keys["gen_ai.output.messages"], "hi there") {
		t.Fatalf("output messages lost the completion: %q", keys["gen_ai.output.messages"])
	}
}

// Content must never leave when capture is off, even though the interceptors
// are wired.
func TestNoContentWhenCaptureDisabled(t *testing.T) {
	cfg := defaultConfig()
	if cfg.CaptureContent {
		t.Fatal("capture_content must default to false")
	}
	tr := &finishedTrace{
		TraceID: "trace-3", RequestID: "req-3", Outcome: "succeeded",
		StartedAt: base, CompletedAt: base.Add(time.Second),
		PromptJSON: []byte(`{"messages":[{"role":"user","content":"secret"}]}`),
		Completion: []byte("secret answer"),
		Attempts:   []usageRecord{{Provider: "claude", Model: "m", RequestedAt: base, Latency: time.Second}},
	}
	spans := buildTraceSpans(tr, cfg, base)
	for _, span := range spans {
		for _, kv := range span.Attributes {
			if strings.Contains(kv.Key, "input.messages") || strings.Contains(kv.Key, "prompt") ||
				strings.Contains(kv.Key, "completion") || strings.Contains(kv.Key, "langfuse.observation.input") {
				t.Fatalf("content attribute %q present with capture disabled", kv.Key)
			}
			if kv.Value.StringValue != nil && (strings.Contains(*kv.Value.StringValue, "secret")) {
				t.Fatalf("secret leaked into %q", kv.Key)
			}
		}
	}
}

func TestParsePromptMessageShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai chat", `{"messages":[{"role":"user","content":"hello"}]}`, "hello"},
		{"anthropic", `{"system":"be nice","messages":[{"role":"user","content":"hi"}]}`, "hi"},
		{"responses string input", `{"input":"plain prompt"}`, "plain prompt"},
		{"responses array input", `{"input":[{"role":"user","content":"arr"}]}`, "arr"},
		{"legacy prompt", `{"prompt":"legacy"}`, "legacy"},
		{"parts array", `{"messages":[{"role":"user","content":[{"type":"text","text":"part text"}]}]}`, "part text"},
	}
	for _, tc := range cases {
		msgs := parsePromptMessages([]byte(tc.body))
		joined := ""
		for _, m := range msgs {
			joined += m.Content
		}
		if !strings.Contains(joined, tc.want) {
			t.Fatalf("%s: got %q, want it to contain %q", tc.name, joined, tc.want)
		}
	}
}

// An unrecognised body must still be preserved rather than silently dropped.
func TestParsePromptKeepsUnknownBody(t *testing.T) {
	msgs := parsePromptMessages([]byte(`{"weird":true}`))
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "weird") {
		t.Fatalf("unrecognised body was dropped: %#v", msgs)
	}
}

func TestDeltaExtractionPerDialect(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai", `data: {"choices":[{"delta":{"content":"abc"}}]}` + "\n\n", "abc"},
		{"anthropic", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"xyz"}}`, "xyz"},
		{"gemini", `data: {"candidates":[{"content":{"parts":[{"text":"gg"}]}}]}`, "gg"},
		{"responses api", `data: {"type":"response.output_text.delta","delta":"rr"}`, "rr"},
		{"role only", `data: {"choices":[{"delta":{"role":"assistant"}}]}`, ""},
		{"usage only", `data: {"usage":{"prompt_tokens":1}}`, ""},
		{"done", `data: [DONE]`, ""},
	}
	for _, tc := range cases {
		if got := extractDeltaText([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCompletionExtractionPerDialect(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"openai", `{"choices":[{"message":{"content":"hey"}}]}`, "hey"},
		{"anthropic", `{"content":[{"type":"text","text":"yo"}]}`, "yo"},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"gm"}]}}]}`, "gm"},
		{"responses", `{"output":[{"content":[{"text":"resp"}]}]}`, "resp"},
	}
	for _, tc := range cases {
		if got := extractCompletionText([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// require_complete must reject half-finished exchanges.
func TestRequireCompleteFiltersUnfinished(t *testing.T) {
	resetPlugin(t)
	cfg := testConfig()
	plugin.config.set(cfg)
	plugin.exp.configure(cfg)

	plugin.store.begin("req-x", base, func(p *pending) {
		p.requestID = "req-x"
		p.setPrompt([]byte(`{"messages":[{"role":"user","content":"q"}]}`), "request")
	})
	raw, _ := handleComplete(mustJSON(t, requestCompletion{
		RequestID: "req-x", TraceID: "trace-x", Outcome: "canceled",
		StartedAt: base, CompletedAt: base.Add(time.Second),
	}))
	if raw == nil {
		t.Fatal("handleComplete returned nothing")
	}
	if traces := plugin.store.drain(); len(traces) != 0 {
		t.Fatalf("canceled request was exported: %d traces", len(traces))
	}
}

func TestCompleteRequestIsExported(t *testing.T) {
	resetPlugin(t)
	cfg := testConfig()
	plugin.config.set(cfg)
	plugin.exp.configure(cfg)

	plugin.store.begin("req-y", base, func(p *pending) {
		p.requestID = "req-y"
		p.setPrompt([]byte(`{"messages":[{"role":"user","content":"q"}]}`), "request")
	})
	plugin.store.appendTo("req-y", base, "answer", "stream")
	_, _ = handleComplete(mustJSON(t, requestCompletion{
		RequestID: "req-y", TraceID: "trace-y", Outcome: "succeeded",
		StartedAt: base, CompletedAt: base.Add(time.Second),
	}))

	traces := plugin.store.drain()
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if string(traces[0].Completion) != "answer" {
		t.Fatalf("completion = %q", traces[0].Completion)
	}
}

func TestSamplingIsPerTrace(t *testing.T) {
	if !sampled("t", "r", 1) {
		t.Fatal("rate 1 must keep everything")
	}
	if sampled("t", "r", 0) {
		t.Fatal("rate 0 must drop everything")
	}
	a := sampled("trace-abc", "req-1", 0.5)
	b := sampled("trace-abc", "req-2", 0.5)
	if a != b {
		t.Fatal("sampling must be decided per trace, not per request id")
	}
}

func TestContentTruncationIsRecorded(t *testing.T) {
	p := &pending{}
	big := strings.Repeat("x", maxStoredCompletionBytes+100)
	p.appendCompletion([]byte(big), "stream")
	if !p.completionTruncated {
		t.Fatal("oversized completion was not marked truncated")
	}
	if len(p.completion) != maxStoredCompletionBytes {
		t.Fatalf("stored %d bytes, want %d", len(p.completion), maxStoredCompletionBytes)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, warnings, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if cfg.CaptureContent {
		t.Fatal("capture_content must default to false")
	}
	if cfg.active() {
		t.Fatal("export must be inactive with no endpoint")
	}
}

func TestConfigHeadersRejectControlChars(t *testing.T) {
	cfg, warnings, err := parseConfig([]byte("headers:\n  Authorization: \"Bearer a\\nb\"\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if _, ok := cfg.Headers["Authorization"]; ok {
		t.Fatal("header with a newline was accepted")
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning")
	}
}

func TestConfigFieldsAreDeclared(t *testing.T) {
	keys := map[string]bool{}
	for _, f := range configFields() {
		keys[f.Name] = true
	}
	for _, want := range []string{"endpoint", "headers", "capture_content", "require_complete", "sample_rate"} {
		if !keys[want] {
			t.Fatalf("config field %q not declared", want)
		}
	}
}

// --- helpers --------------------------------------------------------------

func attrKeys(list []otlpKeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range list {
		switch {
		case kv.Value.StringValue != nil:
			out[kv.Key] = *kv.Value.StringValue
		case kv.Value.IntValue != nil:
			out[kv.Key] = *kv.Value.IntValue
		}
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
