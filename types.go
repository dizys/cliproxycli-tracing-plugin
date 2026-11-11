package main

import (
	"encoding/json"
	"time"
)

// The types below mirror the host's sdk/pluginapi wire structs. Field names are
// the host's, not ours: the RPC decoder matches on these exact strings, and a
// renamed field silently decodes as zero. Only the fields this plugin uses are
// declared; unknown JSON keys are ignored by encoding/json.

// --- registration ---------------------------------------------------------

type pluginRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginMetadata     `json:"metadata"`
	Capabilities  pluginCapabilities `json:"capabilities"`
}

type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type pluginCapabilities struct {
	UsagePlugin            bool `json:"usage_plugin"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	RequestInterceptor     bool `json:"request_interceptor"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
}

type configField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Default     any    `json:"default"`
	Description string `json:"description"`
}

// --- lifecycle ------------------------------------------------------------

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// --- usage ----------------------------------------------------------------

// usageRecord is one upstream attempt: the host emits one per model execution,
// including retries and credential fallbacks.
type usageRecord struct {
	RequestID      string
	TraceID        string
	Provider       string
	BaseURL        string
	ExecutorType   string
	Model          string
	Alias          string
	APIKey         string
	AccessProvider string
	SessionID      string
	AuthID         string
	AuthIndex      string
	AuthType       string
	Source         string
	ResponseModel  string
	Generate       bool
	Stream         bool
	RequestedAt    time.Time
	Latency        time.Duration
	TTFT           time.Duration
	Failed         bool
	Failure        usageFailure
	Detail         usageDetail
}

type usageFailure struct {
	StatusCode int
	Body       string
}

type usageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// --- request completion ---------------------------------------------------

type requestCompletion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
}

// --- interceptors ---------------------------------------------------------

// requestInterceptRequest carries the inbound request. Body is the client's
// payload before translation; that is the one we want for content capture
// because it is the conversation the caller actually wrote.
type requestInterceptRequest struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	ToFormat       string
	Model          string
	RequestedModel string
	Stream         bool
	Body           []byte
	CallerScope    string
}

// streamChunkRequest arrives once per chunk on the hot path. On the header-init
// call (ChunkIndex -1) OriginalRequest carries the full client payload; later
// calls carry only their own delta in Body.
type streamChunkRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	ChunkIndex      int
	Metadata        map[string]any
}

// responseInterceptRequest carries a completed non-streaming response.
type responseInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
	Metadata        map[string]any
}

// responseInterceptResponse must echo nothing when we are only observing: an
// empty Body leaves the response untouched, which is the contract.
type responseInterceptResponse struct{}

type streamChunkResponse struct{}

type requestInterceptResponse struct{}

// emptyResponse is the result body for hooks that return nothing.
type emptyResponse struct{}

func decodeEnvelope[T any](raw []byte) (T, error) {
	var zero T
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, err
	}
	if env.Error != nil {
		return zero, errHostCallFailed
	}
	if len(env.Result) == 0 {
		return zero, nil
	}
	var out T
	if err := json.Unmarshal(env.Result, &out); err != nil {
		return zero, err
	}
	return out, nil
}
