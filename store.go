package main

import (
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
	// sessionID is set when a hook reports one, so a trace whose usage records
	// are late still carries the conversation grouping.
	sessionID string

	// promptJSON is the client's request body, verbatim.
	promptJSON []byte
	// promptTruncated records that the body exceeded maxStoredBodyBytes.
	promptTruncated bool
	// promptFromBody records which hook supplied the prompt, so a missing body
	// can be distinguished from an empty one.
	promptSource string

	// completion accumulates streamed content, or the parts of a non-streaming
	// response. The two are kept apart because a stream has no content array
	// and has to be reassembled from fragments.
	text      []byte
	reasoning []byte
	tools     []streamTool

	// completionParts holds a non-streaming response's parts verbatim.
	completionParts []part

	completionTruncated bool
	completionSource    string

	// chunks counts intercepted deltas, for the span's diagnostics.
	chunks int
	// bytesIn counts delta bytes before truncation, so truncation is visible.
	completionBytesRaw int

	startedAt time.Time
	updatedAt time.Time
}

// streamTool is one tool call being assembled from stream fragments. The host
// delivers a name and id either on the first fragment (OpenAI) or in a separate
// block-start event (Anthropic), and the arguments as fragments after that.
type streamTool struct {
	Index     int
	ID        string
	Name      string
	Arguments string
	// started records that a block-start named this call, so a later fragment
	// with a matching name is not mistaken for a second call.
	started bool
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
	Completion          []part
	CompletionTruncated bool
	CompletionSource    string
	Chunks              int

	// SessionID is the host's session identity for this request, used to group
	// traces into a conversation in the backend.
	SessionID string

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

// appendDelta adds one chunk's content to the assembled completion. It is
// called once per chunk on the request path, so it only appends.
func (p *pending) appendDelta(d streamDelta, source string) {
	if d.empty() {
		return
	}
	if p.completionSource == "" {
		p.completionSource = source
	}
	p.chunks++
	p.completionBytesRaw += len(d.Text) + len(d.Reasoning)
	if d.Text != "" {
		p.text = appendCappedBytes(p, p.text, d.Text)
	}
	if d.Reasoning != "" {
		p.reasoning = appendCappedBytes(p, p.reasoning, d.Reasoning)
	}
	for _, fragment := range d.ToolCalls {
		p.completionBytesRaw += len(fragment.Arguments)
		p.mergeTool(fragment)
	}
}

// appendCappedBytes appends up to the completion budget, flagging truncation
// on the chunk that overflowed it.
//
// This appends into a byte slice rather than concatenating strings. String
// concatenation allocates a fresh copy every call, which for a long stream is
// quadratic in the answer length and dominated the per-chunk cost.
func appendCappedBytes(p *pending, dst []byte, src string) []byte {
	remaining := maxStoredCompletionBytes - len(dst)
	if remaining <= 0 {
		p.completionTruncated = true
		return dst
	}
	if len(src) > remaining {
		p.completionTruncated = true
		return append(dst, src[:remaining]...)
	}
	return append(dst, src...)
}

// mergeTool joins one fragment into the tool call it belongs to.
//
// Two dialects deliver calls differently. OpenAI tags every fragment with an
// index, so the index is authoritative. Anthropic announces the call once with
// its id and name and then streams argument fragments with neither, so a
// fragment carrying a name or id and no index opens a new call, and a bare
// argument fragment attaches to the most recent one.
func (p *pending) mergeTool(fragment toolCallDelta) {
	// An indexed fragment addresses its call directly. This is the OpenAI
	// shape, where parallel calls interleave by index.
	if fragment.HasIndex {
		for len(p.tools) <= fragment.Index {
			p.tools = append(p.tools, streamTool{Index: len(p.tools)})
		}
		t := &p.tools[fragment.Index]
		t.started = true
		if fragment.ID != "" && t.ID == "" {
			t.ID = fragment.ID
		}
		if fragment.Name != "" && t.Name == "" {
			t.Name = fragment.Name
		}
		p.appendToolArguments(t, fragment.Arguments)
		return
	}

	if fragment.Name != "" || fragment.ID != "" {
		// A fragment that names a call opens one, unless it is the same call
		// already open at that index.
		if len(p.tools) > fragment.Index && p.tools[fragment.Index].started {
			t := &p.tools[fragment.Index]
			if fragment.ID != "" && t.ID == "" {
				t.ID = fragment.ID
			}
			if fragment.Name != "" && t.Name == "" {
				t.Name = fragment.Name
			}
			p.appendToolArguments(t, fragment.Arguments)
			return
		}
		t := streamTool{Index: len(p.tools), ID: fragment.ID, Name: fragment.Name, started: true}
		p.appendToolArguments(&t, fragment.Arguments)
		p.tools = append(p.tools, t)
		return
	}

	// A bare argument fragment belongs to the open call.
	if fragment.Arguments == "" {
		return
	}
	if len(p.tools) == 0 {
		// Arguments with no announcement: keep them so nothing is lost.
		t := streamTool{Index: 0}
		p.appendToolArguments(&t, fragment.Arguments)
		p.tools = append(p.tools, t)
		return
	}
	p.appendToolArguments(&p.tools[len(p.tools)-1], fragment.Arguments)
}

func (p *pending) appendToolArguments(t *streamTool, args string) {
	if args == "" {
		return
	}
	if len(t.Arguments)+len(args) > maxStoredCompletionBytes {
		p.completionTruncated = true
		return
	}
	t.Arguments += args
}

// setCompletionParts stores a non-streaming response's parts.
func (p *pending) setCompletionParts(parts []part, source string) {
	if len(parts) == 0 {
		return
	}
	if p.completionSource == "" {
		p.completionSource = source
	}
	p.completionParts = parts
}

// completionSnapshot returns the parts of the assembled completion, whether it
// arrived streamed or whole.
func (p *pending) completionSnapshot() []part {
	if len(p.completionParts) > 0 {
		return p.completionParts
	}
	return streamParts(string(p.text), string(p.reasoning), p.tools)
}

// appendTo adds one chunk's content to a pending request, creating one if the
// header-init call was missed.
func (s *store) appendTo(key string, now time.Time, d streamDelta, source string) {
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
	p.appendDelta(d, source)
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
	// The session id arrives on the usage record rather than on the hooks, so it
	// is taken from the attempts. It groups traces into a conversation.
	sessionID := p.sessionID
	if sessionID == "" {
		for i := range attempts {
			if attempts[i].SessionID != "" {
				sessionID = attempts[i].SessionID
				break
			}
		}
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
		Completion:          p.completionSnapshot(),
		CompletionTruncated: p.completionTruncated,
		CompletionSource:    p.completionSource,
		Chunks:              p.chunks,
		SessionID:           sessionID,
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
