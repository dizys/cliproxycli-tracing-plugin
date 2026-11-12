package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Method names, copied from the host's sdk/pluginabi so the strings cannot drift.
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginShutdown    = "plugin.shutdown"

	methodUsageHandle = "usage.handle"

	methodRequestInterceptBefore = "request.intercept_before"
	methodResponseInterceptAfter = "response.intercept_after"
	methodStreamChunk            = "response.intercept_stream_chunk"
	methodComplete               = "request.complete"
)

const (
	pluginID      = "cliproxy-tracing"
	pluginName    = "CLIProxy Tracing"
	pluginAuthor  = "dizys"
	pluginRepo    = "https://github.com/dizys/cliproxycli-tracing-plugin"
	pluginLogoURL = "https://raw.githubusercontent.com/dizys/cliproxycli-tracing-plugin/main/assets/icon.svg"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=<version>.
var pluginVersion = "0.1.3"

var plugin = newPluginInstance()

// pluginInstance is the process-wide state. The host loads one shared library
// per plugin instance, so package-level state is the correct scope.
type pluginInstance struct {
	store  *store
	exp    *exporter
	config configStore

	mu       sync.Mutex
	attempts map[string][]usageRecord // traceID -> attempts, waiting for completion
	held     []heldTrace              // finished traces waiting out their grace window
	stopped  bool

	looper   loop
	loopOnce sync.Once
}

func newPluginInstance() *pluginInstance {
	p := &pluginInstance{
		store:    newStore(),
		exp:      newExporter(),
		attempts: make(map[string][]usageRecord),
	}
	return p
}

func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return handleRegister(req)
	case methodUsageHandle:
		return handleUsage(req)
	case methodRequestInterceptBefore:
		return handleRequestIntercept(req)
	case methodResponseInterceptAfter:
		return handleResponseIntercept(req)
	case methodStreamChunk:
		return handleStreamChunk(req)
	case methodComplete:
		return handleComplete(req)
	case methodPluginQuiesce:
		plugin.flushNow()
		return okEnvelope(emptyResponse{})
	case methodPluginShutdown:
		plugin.shutdown()
		return okEnvelope(emptyResponse{})
	default:
		// An unknown method is not an error the host can act on; answer
		// successfully so a newer host method does not break the plugin.
		return okEnvelope(emptyResponse{})
	}
}

func handleRegister(req []byte) ([]byte, error) {
	var lifecycle lifecycleRequest
	if len(req) > 0 {
		if err := json.Unmarshal(req, &lifecycle); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg, warnings, err := parseConfig(lifecycle.ConfigYAML)
	if err != nil {
		plugin.log("warn", err.Error())
		cfg = defaultConfig()
	}
	for _, w := range warnings {
		plugin.log("warn", w)
	}
	plugin.config.set(cfg)
	plugin.exp.configure(cfg)
	plugin.startLoop()

	return okEnvelope(pluginRegistration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			Logo:             pluginLogoURL,
			ConfigFields:     configFields(),
		},
		Capabilities: pluginCapabilities{
			UsagePlugin:            true,
			RequestLifecyclePlugin: true,
			// Content capture needs bodies, which only the interceptors carry.
			// They are always declared: capture_content is a runtime switch, and
			// the host wires capabilities at registration only.
			RequestInterceptor:     true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	})
}

// --- hot path -------------------------------------------------------------
// Everything below runs per request or per chunk. The rule is: copy bytes,
// take a lock once, return. No JSON parsing, no allocation beyond the buffer.

// handleRequestIntercept captures the client's request body. This is the
// earliest hook and the one that holds the conversation the caller wrote.
func handleRequestIntercept(req []byte) ([]byte, error) {
	// A request interceptor that returns nothing is a no-op, so this only ever
	// observes. Declining to parse keeps the cost to one buffer copy.
	if len(req) == 0 {
		return okEnvelope(requestInterceptResponse{})
	}
	var r requestInterceptRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return okEnvelope(requestInterceptResponse{})
	}
	if !plugin.config.get().CaptureContent {
		return okEnvelope(requestInterceptResponse{})
	}
	key := firstNonEmpty(r.RequestID, r.TraceID)
	if key == "" {
		return okEnvelope(requestInterceptResponse{})
	}
	plugin.store.begin(key, time.Now(), func(p *pending) {
		p.requestID = r.RequestID
		p.traceID = r.TraceID
		p.sourceFormat = r.SourceFormat
		p.model = r.Model
		p.requestedModel = r.RequestedModel
		p.stream = r.Stream
		p.setPrompt(r.Body, "request")
	})
	return okEnvelope(requestInterceptResponse{})
}

// handleStreamChunk accumulates completion deltas. The header-init call also
// carries the full request body, which is the fallback when the request
// interceptor did not run.
func handleStreamChunk(req []byte) ([]byte, error) {
	if len(req) == 0 {
		return okEnvelope(streamChunkResponse{})
	}
	var r streamChunkRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return okEnvelope(streamChunkResponse{})
	}
	cfg := plugin.config.get()
	if !cfg.CaptureContent {
		return okEnvelope(streamChunkResponse{})
	}
	key := firstNonEmpty(r.RequestID, traceFromMetadata(r.Metadata))
	if key == "" {
		return okEnvelope(streamChunkResponse{})
	}
	now := time.Now()

	if r.ChunkIndex < 0 {
		plugin.store.begin(key, now, func(p *pending) {
			p.requestID = r.RequestID
			p.traceID = traceFromMetadata(r.Metadata)
			p.sourceFormat = r.SourceFormat
			p.model = r.Model
			p.requestedModel = r.RequestedModel
			p.stream = true
			// On header-init the host hands over the full client payload.
			body := r.OriginalRequest
			if len(body) == 0 {
				body = r.RequestBody
			}
			p.setPrompt(body, "stream_header")
		})
		return okEnvelope(streamChunkResponse{})
	}

	text := extractDeltaText(r.Body)
	if len(text) == 0 {
		return okEnvelope(streamChunkResponse{})
	}
	plugin.store.appendTo(key, now, text, "stream")
	return okEnvelope(streamChunkResponse{})
}

// handleResponseIntercept captures a non-streaming response body.
func handleResponseIntercept(req []byte) ([]byte, error) {
	if len(req) == 0 {
		return okEnvelope(responseInterceptResponse{})
	}
	var r responseInterceptRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return okEnvelope(responseInterceptResponse{})
	}
	if !plugin.config.get().CaptureContent {
		return okEnvelope(responseInterceptResponse{})
	}
	key := firstNonEmpty(r.RequestID, traceFromMetadata(r.Metadata))
	if key == "" {
		return okEnvelope(responseInterceptResponse{})
	}
	now := time.Now()
	plugin.store.begin(key, now, func(p *pending) {
		p.requestID = r.RequestID
		p.traceID = traceFromMetadata(r.Metadata)
		p.sourceFormat = r.SourceFormat
		p.model = r.Model
		p.requestedModel = r.RequestedModel
		p.setPrompt(r.OriginalRequest, "response_original")
		if len(p.promptJSON) == 0 {
			p.setPrompt(r.RequestBody, "response_body")
		}
		if text := extractCompletionText(r.Body); len(text) > 0 {
			p.appendCompletion([]byte(text), "response")
		}
	})
	return okEnvelope(responseInterceptResponse{})
}

// handleUsage records one upstream attempt. Attempts are buffered against the
// trace so they can be attached when the request completes.
func handleUsage(req []byte) ([]byte, error) {
	if len(req) == 0 {
		return okEnvelope(emptyResponse{})
	}
	var record usageRecord
	if err := json.Unmarshal(req, &record); err != nil {
		return okEnvelope(emptyResponse{})
	}
	if !record.Generate {
		return okEnvelope(emptyResponse{})
	}
	key := firstNonEmpty(record.TraceID, record.RequestID)
	if key == "" {
		return okEnvelope(emptyResponse{})
	}

	plugin.mu.Lock()
	plugin.attempts[key] = append(plugin.attempts[key], record)
	// Bound the buffer: a trace that never completes must not grow forever.
	if len(plugin.attempts) > maxPendingRequests {
		for k := range plugin.attempts {
			if k != key {
				delete(plugin.attempts, k)
				break
			}
		}
	}
	plugin.mu.Unlock()
	return okEnvelope(emptyResponse{})
}

// handleComplete closes a request and queues it for export.
func handleComplete(req []byte) ([]byte, error) {
	if len(req) == 0 {
		return okEnvelope(emptyResponse{})
	}
	var c requestCompletion
	if err := json.Unmarshal(req, &c); err != nil {
		return okEnvelope(emptyResponse{})
	}
	cfg := plugin.config.get()

	key := firstNonEmpty(c.RequestID, c.TraceID)
	if key == "" {
		return okEnvelope(emptyResponse{})
	}

	plugin.mu.Lock()
	attempts := plugin.attempts[key]
	if len(attempts) == 0 && c.TraceID != "" {
		attempts = plugin.attempts[c.TraceID]
		delete(plugin.attempts, c.TraceID)
	}
	delete(plugin.attempts, key)
	plugin.mu.Unlock()

	// require_complete drops exchanges that did not finish, so an export holds
	// whole conversations rather than truncated fragments.
	if cfg.CaptureContent && cfg.RequireComplete && !completedOK(c) {
		plugin.store.discard(key)
		return okEnvelope(emptyResponse{})
	}
	if !sampled(c.TraceID, c.RequestID, cfg.SampleRate) {
		plugin.store.discard(key)
		return okEnvelope(emptyResponse{})
	}

	plugin.store.finish(key, time.Now(), c, attempts)
	return okEnvelope(emptyResponse{})
}

func completedOK(c requestCompletion) bool {
	switch strings.ToLower(strings.TrimSpace(c.Outcome)) {
	case "", "succeeded", "success":
		return true
	default:
		return false
	}
}

// sampled decides per trace so all spans of one request share the decision.
func sampled(traceID, requestID string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	sum := sha256Sum(firstNonEmpty(traceID, requestID))
	// Use the first two bytes as a 16-bit fraction.
	v := uint16(sum[0])<<8 | uint16(sum[1])
	return float64(v)/65535.0 < rate
}

func traceFromMetadata(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	for _, key := range []string{"trace_id", "TraceID", "x_cpa_trace_id"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
