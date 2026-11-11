package main

import (
	"context"
	"encoding/json"
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
	traces := p.store.drain()
	if len(traces) == 0 {
		p.store.reap(time.Now())
		return
	}

	cfg := p.config.get()
	if !cfg.active() {
		// Nothing configured: the traces are dropped, not queued. This is the
		// default state, so an unconfigured install accumulates nothing.
		return
	}

	now := time.Now()

	// A trace waits CompletionGrace for late usage records before it is built,
	// so an attempt that arrives a moment after the completion still joins.
	p.mu.Lock()
	p.held = append(p.held, heldTrace{traces: traces, readyAt: now.Add(cfg.CompletionGrace)})
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

	if len(ready) == 0 {
		return
	}

	spans := make([]otlpSpan, 0, len(ready)*2)
	for _, tr := range ready {
		spans = append(spans, buildTraceSpans(tr, cfg, now)...)
		if len(spans) >= cfg.MaxBatchSpans {
			p.exportBatch(ctx, cfg, spans)
			spans = spans[:0]
		}
	}
	if len(spans) > 0 {
		p.exportBatch(ctx, cfg, spans)
	}
	p.store.reap(now)
}

func (p *pluginInstance) exportBatch(ctx context.Context, cfg config, spans []otlpSpan) {
	req := otlpRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{Attributes: resourceAttrs(cfg)},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: "cliproxy-tracing", Version: pluginVersion},
				Spans: spans,
			}},
		}},
	}
	if err := p.exp.send(ctx, req); err != nil {
		p.log("warn", "export failed: "+sanitizeError(err))
	}
}

// flushNow runs one flush synchronously, used on quiesce so a shutdown does not
// lose the traces already assembled.
func (p *pluginInstance) flushNow() {
	p.flushOnce(context.Background())
}

func (p *pluginInstance) shutdown() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()

	select {
	case <-p.looper.stop:
	default:
		close(p.looper.stop)
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
