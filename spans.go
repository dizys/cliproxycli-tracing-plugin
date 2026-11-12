package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// OTLP/HTTP JSON wire types. Field names are the OpenTelemetry proto3 JSON
// names, which are fixed by the spec: a wrong name is silently ignored by the
// backend rather than reported, so these are copied from the spec rather than
// chosen.

type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Events            []otlpEvent    `json:"events,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
	// IntValue is a string in proto3 JSON (DecimalString), not a number.
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

const (
	spanKindInternal = 1
	spanKindServer   = 2
	spanKindClient   = 3

	statusError = 2
)

// attrs accumulates span attributes, skipping empty strings so a missing value
// is absent rather than present-and-blank.
type attrs []otlpKeyValue

func (a *attrs) str(key, value string) {
	if value == "" {
		return
	}
	v := value
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{StringValue: &v}})
}

func (a *attrs) integer(key string, value int64) {
	v := strconv.FormatInt(value, 10)
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{IntValue: &v}})
}

func (a *attrs) nonZero(key string, value int64) {
	if value != 0 {
		a.integer(key, value)
	}
}

func (a *attrs) boolean(key string, value bool) {
	v := value
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{BoolValue: &v}})
}

func (a *attrs) double(key string, value float64) {
	v := value
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{DoubleValue: &v}})
}

// buildTraceSpans turns one finished request into a root span plus one child
// span per upstream attempt. This is where content is finally parsed, off the
// request path.
func buildTraceSpans(tr *finishedTrace, cfg config, now time.Time) []otlpSpan {
	tid := traceIDFor(firstNonEmpty(tr.TraceID, tr.RequestID))
	rootID := newSpanID()

	model := firstNonEmpty(tr.RequestedModel, tr.Model)

	var root attrs
	root.str("cpa.trace_id", tr.TraceID)
	root.str("cpa.request_id", tr.RequestID)
	root.str("cpa.outcome", tr.Outcome)
	root.str("cpa.source_format", tr.SourceFormat)
	root.boolean("cpa.stream", tr.Stream)
	root.integer("cpa.attempt_count", int64(len(tr.Attempts)))
	if tr.StatusCode > 0 {
		root.integer("http.response.status_code", int64(tr.StatusCode))
	}

	rootSpan := otlpSpan{
		TraceID:           tid,
		SpanID:            rootID,
		Name:              requestSpanName(model),
		Kind:              spanKindServer,
		StartTimeUnixNano: unixNano(tr.StartedAt),
		EndTimeUnixNano:   unixNano(tr.CompletedAt),
	}

	// A successful request is not always unremarkable: the root carries the
	// outcome so a filtered dashboard can still tell cancellations from errors.
	if tr.Outcome != "" && tr.Outcome != "succeeded" {
		rootSpan.Status = otlpStatus{Code: statusError, Message: tr.Outcome}
		if tr.Error != "" {
			root.str("cpa.error", truncateText(tr.Error, maxErrorAttrBytes))
		}
	}
	// With no attempt recorded there is no child span to carry the
	// conversation, so the root takes it rather than losing it.
	if cfg.CaptureContent && len(tr.Attempts) == 0 {
		addContentAttrs(&root, tr, cfg)
	}
	rootSpan.Attributes = root

	spans := []otlpSpan{rootSpan}

	for i := range tr.Attempts {
		u := &tr.Attempts[i]
		sid := newSpanID()
		var a attrs
		a.str("gen_ai.system", u.Provider)
		a.str("gen_ai.provider.name", u.Provider)
		a.str("gen_ai.request.model", u.Model)

		// PostHog classifies a span as a generation only when this is set, and
		// it is the one attribute the upstream plugin omits. Langfuse's native
		// marker is kept alongside it.
		a.str("gen_ai.operation.name", "chat")
		a.str("langfuse.observation.type", "generation")

		a.str("gen_ai.response.model", firstNonEmpty(u.ResponseModel, u.Model))
		a.integer("gen_ai.usage.input_tokens", u.Detail.InputTokens)
		a.integer("gen_ai.usage.output_tokens", u.Detail.OutputTokens)
		a.nonZero("gen_ai.usage.cached_tokens", u.Detail.CachedTokens)
		a.nonZero("gen_ai.usage.reasoning_tokens", u.Detail.ReasoningTokens)
		a.nonZero("cpa.usage.total_input_tokens", u.Detail.TotalTokens)
		a.str("cpa.provider", u.Provider)
		a.str("cpa.auth_type", u.AuthType)
		a.str("cpa.session_id", u.SessionID)
		if u.TTFT > 0 {
			a.integer("cpa.ttft_ms", u.TTFT.Milliseconds())
		}
		if u.Latency > 0 {
			a.integer("cpa.latency_ms", u.Latency.Milliseconds())
		}

		span := otlpSpan{
			TraceID:           tid,
			SpanID:            sid,
			ParentSpanID:      rootID,
			Name:              attemptSpanName(u),
			Kind:              spanKindClient,
			StartTimeUnixNano: unixNano(u.RequestedAt),
			EndTimeUnixNano:   unixNano(attemptEnd(u)),
		}
		if cfg.CaptureContent && i == len(tr.Attempts)-1 {
			// Content goes on the attempt that produced the answer, and only
			// there. Repeating the same conversation on every retry span
			// multiplies a large payload by the attempt count, which is how an
			// export batch reached the backend's 4 MiB body limit and was
			// rejected whole.
			//
			// Langfuse renders a generation from the observation attributes on
			// whichever span carries them, so the child span is still the
			// generation rather than the root.
			addContentAttrs(&a, tr, cfg)
		}
		if u.Failed {
			if u.Failure.StatusCode > 0 {
				a.integer("http.response.status_code", int64(u.Failure.StatusCode))
			}
			span.Status = otlpStatus{Code: statusError, Message: "upstream attempt failed"}
			if cfg.CaptureContent && u.Failure.Body != "" {
				a.str("cpa.error", truncateText(u.Failure.Body, maxErrorAttrBytes))
			}
		}
		span.Attributes = a
		spans = append(spans, span)
	}
	return spans
}

// addContentAttrs attaches the conversation to a span in every dialect the
// supported backends read.
//
// Content is emitted once per trace, not once per span. A request with three
// upstream attempts would otherwise carry the same multi-hundred-kilobyte
// conversation four times, which is how a batch reaches a backend's body limit.
// The attempt span is the generation for both PostHog and Langfuse, so content
// belongs there; the root keeps only the provenance flags.
func addContentAttrs(a *attrs, tr *finishedTrace, cfg config) {
	prompt := trimPrompt(parsePromptMessages(tr.PromptJSON), cfg)
	out := completionMessages(tr, cfg)

	if len(prompt) > 0 {
		if raw, err := json.Marshal(trimMessages(prompt, cfg)); err == nil {
			text := string(raw)
			// PostHog: gen_ai.input.messages -> $ai_input.
			a.str("gen_ai.input.messages", text)
			// Langfuse native, which takes precedence over the generic form.
			a.str("langfuse.observation.input", text)
		}
	}
	if len(out) > 0 {
		if raw, err := json.Marshal(out); err == nil {
			text := string(raw)
			a.str("gen_ai.output.messages", text)
			a.str("langfuse.observation.output", text)
		}
	}
	if len(prompt) > 0 {
		// Langfuse's generic convention reads these; harmless for PostHog.
		var b strings.Builder
		for i, m := range prompt {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(m.Role)
			b.WriteString(": ")
			b.WriteString(m.Content)
		}
		a.str("gen_ai.prompt", b.String())
	}
	if len(out) > 0 {
		var b strings.Builder
		for i, m := range out {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(m.Content)
		}
		a.str("gen_ai.completion", b.String())
	}

	// Provenance, so a consumer can tell a truncated capture from a short one.
	a.str("cpa.content.prompt_source", tr.PromptSource)
	a.str("cpa.content.completion_source", tr.CompletionSource)
	a.boolean("cpa.content.prompt_truncated", tr.PromptTruncated)
	a.boolean("cpa.content.completion_truncated", tr.CompletionTruncated)
	a.nonZero("cpa.content.chunks", int64(tr.Chunks))
}

// completionMessages shapes the captured completion as an assistant message.
func completionMessages(tr *finishedTrace, cfg config) []chatMessage {
	if len(tr.Completion) == 0 {
		return nil
	}
	text := string(tr.Completion)
	if cap := cfg.MaxTraceContentBytes; cap > 0 && len(text) > cap {
		text = text[:cap] + "\n...[truncated]"
	}
	return []chatMessage{{Role: "assistant", Content: text}}
}

// trimPrompt applies the per-trace content cap, then the per-message cap.
//
// A long agent conversation is over the backend's body limit long before it is
// over any per-message limit: twenty turns of tool traffic at 40 KiB each is
// 800 KiB in one span. The oldest messages are dropped rather than the newest,
// because the most recent turns are the ones that describe the exchange being
// traced, and the removal is marked so a consumer can tell a short conversation
// from a clipped one.
func trimPrompt(msgs []chatMessage, cfg config) []chatMessage {
	cap := cfg.MaxTraceContentBytes
	if cap <= 0 || len(msgs) == 0 {
		return trimMessages(msgs, cfg)
	}

	total := 0
	for _, m := range msgs {
		total += len(m.Content)
	}
	// Keep the tail: drop from the front until it fits, always leaving the
	// newest message.
	drop := 0
	for total > cap && drop < len(msgs)-1 {
		total -= len(msgs[drop].Content)
		drop++
	}
	if drop > 0 {
		msgs = msgs[drop:]
		// Say that a prefix was removed, so the first remaining turn is not
		// mistaken for the start of the conversation.
		msgs[0] = chatMessage{Role: msgs[0].Role, Content: "...[earlier messages omitted]\n" + msgs[0].Content}
		total = 0
		for _, m := range msgs {
			total += len(m.Content)
		}
	}
	// One message can still be over the cap on its own, usually a pasted file.
	// Left alone it would make a span larger than the budget, so it is clipped.
	if total > cap && len(msgs) == 1 {
		msgs[0].Content = truncateText(msgs[0].Content, cap)
	}
	return trimMessages(msgs, cfg)
}

// trimMessages applies the configured per-content cap so a huge prompt does not
// turn into a multi-megabyte attribute. Truncation is signalled separately.
func trimMessages(msgs []chatMessage, cfg config) []chatMessage {
	if cfg.MaxContentChars <= 0 {
		return msgs
	}
	out := make([]chatMessage, len(msgs))
	for i, m := range msgs {
		if len(m.Content) > cfg.MaxContentChars {
			out[i] = chatMessage{Role: m.Role, Content: m.Content[:cfg.MaxContentChars] + "...[truncated]"}
			continue
		}
		out[i] = m
	}
	return out
}

const maxErrorAttrBytes = 1024

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func requestSpanName(model string) string {
	if model = strings.TrimSpace(model); model != "" {
		return "cpa.request " + model
	}
	return "cpa.request"
}

func attemptSpanName(u *usageRecord) string {
	if p := strings.TrimSpace(u.Provider); p != "" {
		return "cpa.attempt " + p
	}
	return "cpa.attempt"
}

func attemptEnd(u *usageRecord) time.Time {
	if u.Latency <= 0 {
		return u.RequestedAt
	}
	return u.RequestedAt.Add(u.Latency)
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

// traceIDFor derives a 16-byte trace id from the host's trace or request id, so
// all spans of one request share a trace across exports.
func traceIDFor(seed string) string {
	if seed == "" {
		return newSpanID() + newSpanID()[:16]
	}
	var out [16]byte
	sum := sha256Sum(seed)
	copy(out[:], sum[:16])
	return hex.EncodeToString(out[:])
}

func newSpanID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf[:])
}
