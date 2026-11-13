package main

import (
	"encoding/json"
	"testing"
	"time"
)

// The hot-path guarantee: a stream chunk must be handled with one JSON decode
// and no repeated work, so a long generation does not accumulate cost.
func BenchmarkStreamChunkHotPath(b *testing.B) {
	plugin = newPluginInstance()
	cfg := testConfig()
	plugin.config.set(cfg)

	chunk := map[string]any{
		"RequestID":  "req-bench",
		"ChunkIndex": 1,
		"Model":      "claude-opus-5-5",
		"Body":       []byte(`data: {"choices":[{"delta":{"content":"a chunk of generated text"}}]}`),
		"Metadata":   map[string]any{"trace_id": "trace-bench"},
	}
	payload, _ := json.Marshal(chunk)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := handleStreamChunk(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// A chunk with no text (role announcement, usage object) must be rejected
// before a JSON parse, which is the common case in a long stream.
func BenchmarkStreamChunkNoText(b *testing.B) {
	plugin = newPluginInstance()
	cfg := testConfig()
	plugin.config.set(cfg)

	chunk := map[string]any{
		"RequestID":  "req-bench2",
		"ChunkIndex": 1,
		"Body":       []byte(`data: {"id":"x","object":"chat.completion.chunk","created":1}`),
	}
	payload, _ := json.Marshal(chunk)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := handleStreamChunk(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// Capture disabled must be cheaper still: no body copy, no extraction.
func BenchmarkStreamChunkCaptureOff(b *testing.B) {
	plugin = newPluginInstance()
	cfg := testConfig()
	cfg.CaptureContent = false
	plugin.config.set(cfg)

	chunk := map[string]any{
		"RequestID":  "req-bench3",
		"ChunkIndex": 1,
		"Body":       []byte(`data: {"choices":[{"delta":{"content":"text"}}]}`),
	}
	payload, _ := json.Marshal(chunk)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := handleStreamChunk(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// Span building is off the request path, so it is allowed to be slower; this
// pins roughly where it sits so a regression is visible.
func BenchmarkBuildSpans(b *testing.B) {
	tr := &finishedTrace{
		TraceID: "t", RequestID: "r", Outcome: "succeeded",
		StartedAt: base, CompletedAt: base.Add(time.Second),
		PromptJSON: []byte(`{"messages":[{"role":"user","content":"` + string(make([]byte, 0)) + `hello"}]}`),
		Completion: []part{{Type: partText, Content: "hi there"}},
		Attempts:   []usageRecord{{Provider: "claude", Model: "m", RequestedAt: base, Latency: time.Second}},
	}
	cfg := testConfig()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buildTraceSpans(tr, cfg, base)
	}
}
