package main

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// The design constraint for this plugin is latency. Every hook the host offers
// that can see content runs on the request path, so anything slow there is slow
// for the caller. The rule used throughout: the hot path copies bytes and does
// nothing else; every parse happens on the exporter goroutine.
//
// That means the prompt body is stored as the raw JSON the client sent, and the
// completion is stored as the concatenated stream delta text. Both are parsed
// exactly once, when the span is built, after the request is already answered.

const (
	// maxPendingRequests bounds partially-assembled requests. A request that
	// never completes (crashed worker, dropped connection) would otherwise pin
	// its buffers forever.
	maxPendingRequests = 2048

	// maxStoredBodyBytes bounds one stored body. Content above this is dropped
	// rather than parsed, because a multi-megabyte body would both cost parse
	// time and blow past the exporter's batch limit.
	maxStoredBodyBytes = 4 << 20

	// maxStoredCompletionBytes bounds the assembled completion text for one
	// request. Long generations are truncated at the tail, which is recorded in
	// the span so a consumer can tell a truncation from a short answer.
	maxStoredCompletionBytes = 2 << 20

	// pendingTTL drops requests whose completion never arrived. The host sends
	// request.complete for every request that reached interception, so a stale
	// entry means something went wrong upstream of us.
	pendingTTL = 15 * time.Minute
)

// pending is one in-flight request, assembled from the hooks and released when
// the request completes. Buffers are raw: nothing here is parsed.
type pending struct {
	traceID        string
	requestID      string
	sourceFormat   string
	model          string
	requestedModel string
	stream         bool

	// promptJSON is the client's request body, verbatim.
	promptJSON []byte
	// promptTruncated records that the body exceeded maxStoredBodyBytes.
	promptTruncated bool
	// promptFromBody records which hook supplied the prompt, so a missing body
	// can be distinguished from an empty one.
	promptSource string

	// completion accumulates streamed delta text or the whole non-stream body.
	completion          []byte
	completionTruncated bool
	completionSource    string

	// chunks counts intercepted deltas, for the span's diagnostics.
	chunks int
	// bytesIn counts delta bytes before truncation, so truncation is visible.
	completionBytesRaw int

	startedAt time.Time
	updatedAt time.Time
}

// store holds in-flight requests and finished traces waiting to be exported.
type store struct {
	mu sync.Mutex

	pending map[string]*pending
	// traceIndex maps the host trace id to the key used in pending. The hooks
	// use different id spaces, so every lookup tries both.
	traceIndex map[string]string

	// finished holds ready-to-export traces. The exporter drains it.
	finished []*finishedTrace

	dropped uint64
}

// finishedTrace is one completed request, ready to become a span.
type finishedTrace struct {
	TraceID        string
	RequestID      string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time

	PromptJSON          []byte
	PromptTruncated     bool
	PromptSource        string
	Completion          []byte
	CompletionTruncated bool
	CompletionSource    string
	Chunks              int

	// Attempts are the usage records for this trace, in arrival order.
	Attempts []usageRecord
}

func newStore() *store {
	return &store{
		pending:    make(map[string]*pending),
		traceIndex: make(map[string]string),
	}
}

// keyFor resolves the pending key for a request, preferring the request id and
// falling back to the trace id.
func (s *store) keyFor(requestID, traceID string) (string, bool) {
	if requestID != "" {
		if _, ok := s.pending[requestID]; ok {
			return requestID, true
		}
	}
	if traceID != "" {
		if key, ok := s.traceIndex[traceID]; ok {
			if _, ok := s.pending[key]; ok {
				return key, true
			}
		}
	}
	return "", false
}

// begin registers a request and records its prompt. Called from the request
// interceptor (or the stream header-init call when that arrives first).
func (s *store) begin(key string, now time.Time, seed func(*pending)) *pending {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p, ok := s.pending[key]; ok {
		seed(p)
		p.updatedAt = now
		return p
	}
	if len(s.pending) >= maxPendingRequests {
		s.evictOldestLocked()
	}
	p := &pending{startedAt: now, updatedAt: now}
	seed(p)
	s.pending[key] = p
	if p.traceID != "" {
		s.traceIndex[p.traceID] = key
	}
	return p
}

// evictOldestLocked drops the least recently updated request to make room.
func (s *store) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, p := range s.pending {
		if oldestKey == "" || p.updatedAt.Before(oldest) {
			oldestKey, oldest = key, p.updatedAt
		}
	}
	if oldestKey == "" {
		return
	}
	if p := s.pending[oldestKey]; p != nil && p.traceID != "" {
		delete(s.traceIndex, p.traceID)
	}
	delete(s.pending, oldestKey)
	s.dropped++
}

// setPrompt stores the client request body verbatim, capped. It never
// overwrites a body already stored, because the earliest hook to see it holds
// the client's original payload rather than a translated one.
func (p *pending) setPrompt(body []byte, source string) {
	if len(body) == 0 || len(p.promptJSON) > 0 {
		return
	}
	p.promptSource = source
	if len(body) > maxStoredBodyBytes {
		p.promptJSON = append([]byte(nil), body[:maxStoredBodyBytes]...)
		p.promptTruncated = true
		return
	}
	p.promptJSON = append([]byte(nil), body...)
}

// appendCompletion adds streamed delta text or a whole response body.
func (p *pending) appendCompletion(text []byte, source string) {
	if len(text) == 0 {
		return
	}
	if p.completionSource == "" {
		p.completionSource = source
	}
	p.completionBytesRaw += len(text)
	remaining := maxStoredCompletionBytes - len(p.completion)
	if remaining <= 0 {
		p.completionTruncated = true
		return
	}
	if len(text) > remaining {
		p.completion = append(p.completion, text[:remaining]...)
		p.completionTruncated = true
		return
	}
	p.completion = append(p.completion, text...)
}

// appendTo adds completed delta text to a pending request, creating one if the
// header-init call was missed.
func (s *store) appendTo(key string, now time.Time, text string, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[key]
	if !ok {
		if len(s.pending) >= maxPendingRequests {
			s.evictOldestLocked()
		}
		p = &pending{startedAt: now}
		s.pending[key] = p
	}
	p.updatedAt = now
	p.chunks++
	p.appendCompletion([]byte(text), source)
}

// discard drops a pending request without exporting it, used when sampling or
// the completeness filter rejects the trace.
func (s *store) discard(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[key]; ok {
		if p.traceID != "" {
			delete(s.traceIndex, p.traceID)
		}
		delete(s.pending, key)
	}
}

// finish closes a request and hands it to the exporter. Any attempt records
// already buffered for it are attached.
func (s *store) finish(key string, now time.Time, c requestCompletion, attempts []usageRecord) {
	s.mu.Lock()
	p, ok := s.pending[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.pending, key)
	if p.traceID != "" {
		delete(s.traceIndex, p.traceID)
	}
	tr := &finishedTrace{
		TraceID:             firstNonEmpty(c.TraceID, p.traceID),
		RequestID:           firstNonEmpty(c.RequestID, p.requestID),
		SourceFormat:        firstNonEmpty(c.SourceFormat, p.sourceFormat),
		Model:               firstNonEmpty(c.Model, p.model),
		RequestedModel:      firstNonEmpty(c.RequestedModel, p.requestedModel),
		Stream:              c.Stream || p.stream,
		Outcome:             c.Outcome,
		StatusCode:          c.StatusCode,
		Error:               c.Error,
		StartedAt:           c.StartedAt,
		CompletedAt:         c.CompletedAt,
		PromptJSON:          p.promptJSON,
		PromptTruncated:     p.promptTruncated,
		PromptSource:        p.promptSource,
		Completion:          p.completion,
		CompletionTruncated: p.completionTruncated,
		CompletionSource:    p.completionSource,
		Chunks:              p.chunks,
		Attempts:            attempts,
	}
	if tr.StartedAt.IsZero() {
		tr.StartedAt = p.startedAt
	}
	if tr.CompletedAt.IsZero() {
		tr.CompletedAt = now
	}
	s.finished = append(s.finished, tr)
	s.mu.Unlock()
}

// drain returns and clears finished traces.
func (s *store) drain() []*finishedTrace {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.finished) == 0 {
		return nil
	}
	out := s.finished
	s.finished = nil
	return out
}

// reap drops requests that have not been touched within pendingTTL.
func (s *store) reap(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, p := range s.pending {
		if now.Sub(p.updatedAt) < pendingTTL {
			continue
		}
		if p.traceID != "" {
			delete(s.traceIndex, p.traceID)
		}
		delete(s.pending, key)
		s.dropped++
	}
}

// stats reports counters for the health route.
func (s *store) stats() (pendingCount int, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), s.dropped
}

// parsePromptMessages turns the stored client body into the message list every
// backend understands. This runs on the exporter goroutine, never inline.
//
// The body shape differs by protocol, so this accepts the common shapes rather
// than assuming one: OpenAI chat/completions uses "messages", the Responses API
// uses "input" (a string or an array), and Anthropic uses "messages" with a
// separate "system" string. Anything unrecognised is kept as a single raw
// message so no content is silently lost.
func parsePromptMessages(body []byte) []chatMessage {
	if len(body) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return []chatMessage{{Role: "user", Content: string(body)}}
	}

	var out []chatMessage
	if system := stringField(doc, "system"); system != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}
	if msgs, ok := doc["messages"].([]any); ok {
		for _, item := range msgs {
			if m, ok := item.(map[string]any); ok {
				out = append(out, chatMessage{
					Role:    stringField(m, "role"),
					Content: contentToString(m["content"]),
				})
			}
		}
	}
	if len(out) == 0 {
		switch input := doc["input"].(type) {
		case string:
			if input != "" {
				out = append(out, chatMessage{Role: "user", Content: input})
			}
		case []any:
			for _, item := range input {
				if m, ok := item.(map[string]any); ok {
					out = append(out, chatMessage{
						Role:    stringField(m, "role"),
						Content: contentToString(m["content"]),
					})
				}
			}
		}
	}
	if len(out) == 0 {
		if prompt := stringField(doc, "prompt"); prompt != "" {
			out = append(out, chatMessage{Role: "user", Content: prompt})
		}
	}
	if len(out) == 0 {
		// Preserve the payload rather than dropping it.
		out = append(out, chatMessage{Role: "user", Content: string(body)})
	}
	for i := range out {
		if out[i].Role == "" {
			out[i].Role = "user"
		}
	}
	return out
}

// contentToString flattens a message content field. Providers allow either a
// string or an array of typed parts (text, image_url, tool_use...), so the
// array form is folded into readable text and non-text parts are labelled.
func contentToString(v any) string {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		out := make([]byte, 0, 256)
		for i, part := range typed {
			if m, ok := part.(map[string]any); ok {
				typ := stringField(m, "type")
				if text := stringField(m, "text"); text != "" {
					if i > 0 {
						out = append(out, '\n')
					}
					out = append(out, text...)
					continue
				}
				if typ != "" {
					if i > 0 {
						out = append(out, '\n')
					}
					out = append(out, '[')
					out = append(out, typ...)
					out = append(out, ']')
				}
				continue
			}
			if s, ok := part.(string); ok {
				out = append(out, s...)
			}
		}
		return string(out)
	default:
		// Numbers, bools and objects that are not message parts.
		if raw, err := json.Marshal(typed); err == nil {
			return string(raw)
		}
		return ""
	}
}

func stringField(doc map[string]any, key string) string {
	if v, ok := doc[key].(string); ok {
		return v
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sortedKeys is used by tests that need deterministic ordering.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// chatMessage is the neutral message shape both PostHog and Langfuse read.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
