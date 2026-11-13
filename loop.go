package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// configStore holds the current config so hooks can read it without touching
// the exporter's mutex on the hot path.
type configStore struct {
	mu  sync.RWMutex
	cfg config
}

func (s *configStore) set(cfg config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

func (s *configStore) get() config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// loop runs the exporter: drain finished traces, build spans, batched export.
// Everything expensive happens here rather than in a hook.
type loop struct {
	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}
}

func (p *pluginInstance) startLoop() {
	p.loopOnce.Do(func() {
		p.looper.stop = make(chan struct{})
		p.looper.done = make(chan struct{})
		go p.runLoop()
	})
}

// runLoop batches finished traces and exports them on the flush interval.
func (p *pluginInstance) runLoop() {
	defer close(p.looper.done)
	for {
		cfg := p.config.get()
		interval := cfg.FlushInterval
		if interval <= 0 {
			interval = time.Second
		}
		select {
		case <-p.looper.stop:
			p.flushOnce(context.Background())
			return
		case <-time.After(interval):
			p.flushOnce(context.Background())
		}
	}
}

// flushOnce drains and sends one round of spans.
func (p *pluginInstance) flushOnce(ctx context.Context) {
	cfg := p.config.get()
	now := time.Now()

	// Spans whose earlier export failed go out first, before new ones, so the
	// retry queue does not grow behind a steady stream of fresh traffic.
	retry := p.exp.takeQueued()

	if !cfg.active() || !p.exp.ready(now) {
		// Nothing configured, or the endpoint is inside its backoff window.
		// Queued spans are kept rather than flushed into a failing backend, and
		// traces stay in the store until a later tick.
		if len(retry) > 0 {
			p.exp.requeue(retry)
		}
		p.store.reap(now)
		return
	}

	traces := p.store.drain()

	// A trace waits CompletionGrace for late usage records before it is built,
	// so an attempt that arrives a moment after the completion still joins.
	p.mu.Lock()
	if len(traces) > 0 {
		p.held = append(p.held, heldTrace{traces: traces, readyAt: now.Add(cfg.CompletionGrace)})
	}
	ready := make([]*finishedTrace, 0, len(p.held))
	stillHeld := p.held[:0]
	for _, h := range p.held {
		if now.Before(h.readyAt) {
			stillHeld = append(stillHeld, h)
			continue
		}
		ready = append(ready, h.traces...)
	}
	p.held = stillHeld
	p.mu.Unlock()

	// Retries go first: they are already behind, and sending them ahead of new
	// traffic keeps the queue from growing behind a steady stream.
	all := make([]otlpSpan, 0, len(retry)+len(ready)*2)
	all = append(all, retry...)
	for _, tr := range ready {
		all = append(all, buildTraceSpans(tr, cfg, now)...)
	}

	for _, batch := range splitBatches(cfg, all) {
		p.sendBatch(ctx, cfg, batch)
	}
	p.store.reap(now)
}

// encodedBatch is one export request. Each span is marshalled once: the bytes
// are reused to measure the batch, to assemble it, and to post it.
type encodedBatch struct {
	spans []otlpSpan
	parts [][]byte
}

// body assembles the OTLP envelope around the already-encoded spans.
func (b encodedBatch) body(cfg config) []byte {
	head, _ := json.Marshal(otlpRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{Attributes: resourceAttrs(cfg)},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: "cliproxy-tracing", Version: pluginVersion},
				Spans: []otlpSpan{},
			}},
		}},
	})
	// head ends its span list with an empty "spans":[]; splice the encoded
	// spans into it rather than re-marshalling the whole request.
	marker := []byte(`"spans":[]`)
	idx := bytes.Index(head, marker)
	if idx < 0 {
		raw, _ := json.Marshal(encodeBatch(cfg, b.spans))
		return raw
	}
	size := len(head)
	for _, p := range b.parts {
		size += len(p) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, head[:idx]...)
	out = append(out, `"spans":[`...)
	for i, p := range b.parts {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, p...)
	}
	out = append(out, ']')
	out = append(out, head[idx+len(marker):]...)
	return out
}

// splitBatches divides spans into export requests that each fit under the
// backend's body limit.
//
// A count-only limit is not enough: 512 spans of an agent conversation is many
// megabytes, and a backend rejects an oversized request whole, losing every
// span in it. JSON escaping also expands content, so the size has to be
// measured after encoding rather than estimated from the raw strings.
func splitBatches(cfg config, spans []otlpSpan) []encodedBatch {
	if len(spans) == 0 {
		return nil
	}
	budget := cfg.MaxBatchBytes
	if budget <= 0 || budget > maxExportBytes {
		budget = defaultExportBudget
	}
	maxSpans := cfg.MaxBatchSpans
	if maxSpans <= 0 {
		maxSpans = 512
	}

	var out []encodedBatch
	cur := encodedBatch{}
	size := envelopeBytes

	for _, span := range spans {
		raw, err := json.Marshal(span)
		if err != nil {
			continue
		}
		n := len(raw) + 1 // the comma joining it to the previous span
		// A single span over the whole budget still has to go somewhere, so it
		// is sent alone rather than dropped; only the backend can refuse it.
		if len(cur.spans) > 0 && (len(cur.spans) >= maxSpans || size+n > budget) {
			out = append(out, cur)
			cur = encodedBatch{}
			size = envelopeBytes
		}
		cur.spans = append(cur.spans, span)
		cur.parts = append(cur.parts, raw)
		size += n
	}
	if len(cur.spans) > 0 {
		out = append(out, cur)
	}
	return out
}

// envelopeBytes is what the resource, scope and JSON framing add to a batch
// that holds no spans. Measured from the real encoding rather than guessed.
var envelopeBytes = func() int {
	raw, _ := json.Marshal(encodeBatch(defaultConfig(), []otlpSpan{}))
	return len(raw)
}()

func (p *pluginInstance) exportBatch(ctx context.Context, cfg config, spans []otlpSpan) {
	for _, batch := range splitBatches(cfg, spans) {
		p.sendBatch(ctx, cfg, batch)
	}
}

// sendBatch posts one encoded batch and routes a failure.
func (p *pluginInstance) sendBatch(ctx context.Context, cfg config, batch encodedBatch) {
	if len(batch.spans) == 0 {
		return
	}
	if err := p.exp.sendEncoded(ctx, batch.body(cfg)); err != nil {
		p.handleSendError(ctx, cfg, batch, err)
	}
}

// handleSendError routes a failed export. A 413 is the one rejection a smaller
// batch survives, so it is split; a transient failure is requeued; anything
// else permanent is reported and dropped.
func (p *pluginInstance) handleSendError(ctx context.Context, cfg config, batch encodedBatch, err error) {
	var permanent *permanentError
	if errors.As(err, &permanent) {
		if permanent.isPayloadTooLarge() && len(batch.spans) > 1 {
			mid := len(batch.spans) / 2
			for _, half := range splitBatches(cfg, batch.spans[:mid]) {
				p.sendBatch(ctx, cfg, half)
			}
			for _, half := range splitBatches(cfg, batch.spans[mid:]) {
				p.sendBatch(ctx, cfg, half)
			}
			return
		}
		if permanent.isPayloadTooLarge() {
			// One span larger than the backend's whole request limit. It can
			// never be sent, so say so rather than retrying forever.
			p.log("warn", "export rejected, span too large for the backend: "+sanitizeError(err))
			return
		}
		p.log("warn", "export rejected: "+sanitizeError(err))
		return
	}
	// Transient (a network error, a 5xx, a rate limit): the spans go back on
	// the queue to be retried once the backoff expires, rather than being
	// dropped. This is what stops a rolling backend restart from losing every
	// trace that happened during it.
	p.exp.requeue(batch.spans)
	p.log("warn", "export failed, will retry: "+sanitizeError(err))
}

// encodeBatch wraps spans in the OTLP resource and scope envelope.
func encodeBatch(cfg config, spans []otlpSpan) otlpRequest {
	return otlpRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{Attributes: resourceAttrs(cfg)},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: "cliproxy-tracing", Version: pluginVersion},
				Spans: spans,
			}},
		}},
	}
}

// flushNow drains everything and exports it, ignoring the grace window. This is
// the last-chance path on quiesce and shutdown: a trace inside its grace window
// would otherwise be discarded, because the loop that would have released it is
// about to stop.
func (p *pluginInstance) flushNow() {
	p.releaseHeld(true)
	p.flushOnce(context.Background())
}

// releaseHeld moves traces still inside their grace window into the ready set,
// or drops them when force is false and they have not expired yet.
func (p *pluginInstance) releaseHeld(force bool) {
	if !force {
		return
	}
	p.mu.Lock()
	if len(p.held) == 0 {
		p.mu.Unlock()
		return
	}
	pending := p.held
	p.held = nil
	p.mu.Unlock()

	for _, h := range pending {
		for _, tr := range h.traces {
			p.exportTrace(context.Background(), tr)
		}
	}
}

// exportTrace builds and sends one trace immediately, outside the batch budget.
func (p *pluginInstance) exportTrace(ctx context.Context, tr *finishedTrace) {
	cfg := p.config.get()
	if !cfg.active() {
		return
	}
	for _, batch := range splitBatches(cfg, buildTraceSpans(tr, cfg, time.Now())) {
		p.sendBatch(ctx, cfg, batch)
	}
}

func (p *pluginInstance) shutdown() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	retired := p.looper.stop
	p.mu.Unlock()

	// The host calls shutdown during its preflight probe, before any register,
	// so the loop may never have started and both channels are nil. Closing a
	// nil channel panics, which is fatal inside dlopen.
	if retired == nil {
		return
	}
	select {
	case <-retired:
	default:
		close(retired)
	}
	select {
	case <-p.looper.done:
	case <-time.After(5 * time.Second):
	}
}

// resourceAttrs describes the exporter to the backend.
func resourceAttrs(cfg config) []otlpKeyValue {
	var a attrs
	a.str("service.name", cfg.ServiceName)
	a.str("service.version", pluginVersion)
	a.str("telemetry.sdk.name", "cliproxy-tracing")
	a.str("telemetry.sdk.language", "go")
	return a
}

// log forwards a message to the host log so problems appear in the server log
// rather than vanishing.
func (p *pluginInstance) log(level, message string) {
	payload, err := json.Marshal(map[string]any{
		"level":   level,
		"message": "[cliproxy-tracing] " + message,
	})
	if err != nil {
		return
	}
	_, _ = callHost("host.log", payload)
}

// heldTrace is a finished request waiting out its grace window.
type heldTrace struct {
	traces  []*finishedTrace
	readyAt time.Time
}
