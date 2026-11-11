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

// An agent transcript is mostly tool traffic. A tool_use carries the arguments
// the model emitted and a tool_result carries what the tool returned; both were
// previously replaced with a bare [tool_use] label, losing the payload.
func TestToolCallPayloadsArePreserved(t *testing.T) {
	body := `{"messages":[
      {"role":"assistant","content":[
        {"type":"text","text":"Let me look."},
        {"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"/etc/hosts","limit":50}}
      ]},
      {"role":"user","content":[
        {"type":"tool_result","tool_use_id":"toolu_1","content":"127.0.0.1 localhost"}
      ]},
      {"role":"assistant","content":[
        {"type":"thinking","thinking":"The file lists loopback."}
      ]}
    ]}`
	msgs := parsePromptMessages([]byte(body))
	all := ""
	for _, m := range msgs {
		all += m.Content + "\n"
	}

	for _, want := range []string{"/etc/hosts", "read_file", "127.0.0.1 localhost", "The file lists loopback"} {
		if !strings.Contains(all, want) {
			t.Fatalf("tool payload %q was dropped; got:\n%s", want, all)
		}
	}
	if strings.Contains(all, "[tool_use]\n") || strings.TrimSpace(all) == "[tool_use]" {
		t.Fatal("tool_use was replaced with a bare label")
	}
}

// Streamed tool calls arrive as name-then-argument-fragments with no text.
func TestStreamedToolCallFragmentsArePreserved(t *testing.T) {
	nameChunk := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`
	argChunk := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}}]}`

	if got := extractDeltaText([]byte(nameChunk)); !strings.Contains(got, "get_weather") {
		t.Fatalf("tool name dropped: %q", got)
	}
	if got := extractDeltaText([]byte(argChunk)); !strings.Contains(got, "Paris") {
		t.Fatalf("tool arguments dropped: %q", got)
	}
}

// Anthropic streams tool input and thinking as their own delta types.
func TestAnthropicToolAndThinkingDeltas(t *testing.T) {
	inputChunk := `data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`
	thinkingChunk := `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`

	if got := extractDeltaText([]byte(inputChunk)); !strings.Contains(got, `"q":"x"`) {
		t.Fatalf("tool input dropped: %q", got)
	}
	if got := extractDeltaText([]byte(thinkingChunk)); !strings.Contains(got, "hmm") {
		t.Fatalf("thinking dropped: %q", got)
	}
}

// Non-streaming responses carry tool_calls on the message, not in content.
func TestNonStreamingToolCallsArePreserved(t *testing.T) {
	body := `{"choices":[{"message":{"content":null,"tool_calls":[
      {"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hedgehogs\"}"}}
    ]}}]}`
	got := extractCompletionText([]byte(body))
	if !strings.Contains(got, "search") || !strings.Contains(got, "hedgehogs") {
		t.Fatalf("non-streaming tool call dropped: %q", got)
	}
}

func TestPlainTextStillClean(t *testing.T) {
	msgs := parsePromptMessages([]byte(`{"messages":[{"role":"user","content":"just text"}]}`))
	if len(msgs) != 1 || msgs[0].Content != "just text" {
		t.Fatalf("plain text was mangled: %#v", msgs)
	}
	// A text part must not gain a [text] prefix.
	parts := parsePromptMessages([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	if parts[0].Content != "hi" {
		t.Fatalf("text part = %q, want it verbatim", parts[0].Content)
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

// The host calls plugin.shutdown during its dlopen preflight, before any
// register, so the loop never started and its channels are nil. Closing a nil
// channel panics, and a panic inside dlopen makes the plugin fail to load.
func TestShutdownBeforeRegisterIsSafe(t *testing.T) {
	resetPlugin(t)
	// No startLoop: simulate the preflight probe.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("shutdown panicked before register: %v", r)
		}
	}()
	plugin.shutdown()
	// A second shutdown from the real unload must also be safe.
	plugin.shutdown()
}

// The same preflight calls quiesce before any config exists.
func TestQuiesceBeforeRegisterIsSafe(t *testing.T) {
	resetPlugin(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("quiesce panicked before register: %v", r)
		}
	}()
	if _, err := handleMethod(methodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
}

// Register then shutdown is the normal unload path and must not hang.
func TestShutdownAfterRegisterCompletes(t *testing.T) {
	resetPlugin(t)
	if _, err := handleMethod(methodPluginRegister, []byte(`{"schema_version":6}`)); err != nil {
		t.Fatalf("register: %v", err)
	}
	done := make(chan struct{})
	go func() {
		plugin.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown after register did not complete")
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
