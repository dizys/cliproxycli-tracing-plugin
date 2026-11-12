package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// One export attempt may not block the loop for longer than this.
	maxExportTimeout = 30 * time.Second
	// PostHog and comparable OTLP backends reject a request body above 4 MiB
	// with a 413 and discard every span in it, so no batch may be allowed to
	// approach that size. maxExportBytes is the hard ceiling the config may set;
	// the default budget is lower, leaving room for the resource and scope
	// envelope and for a backend with a slightly different limit.
	maxExportBytes      = 4 << 20
	defaultExportBudget = 3 << 20
	// The queue is bounded so a failing endpoint cannot grow memory without
	// limit; oldest spans go first and the drop is counted.
	maxQueuedSpans = 20000
	// Backoff between retries of a failing export.
	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 30 * time.Second
	// Response bodies are only read to log an error, so cap them.
	maxResponseBytes = 64 << 10
)

var errHostCallFailed = errors.New("host call failed")

// permanentError is a rejection retrying the same payload cannot fix: an
// oversized body, a bad token, a malformed attribute. The caller may still
// recover by splitting the batch, but a straight resend is pointless.
type permanentError struct {
	StatusCode int
	Message    string
}

func (e *permanentError) Error() string { return e.Message }

// isPayloadTooLarge reports the one permanent error a smaller batch survives.
func (e *permanentError) isPayloadTooLarge() bool {
	return e.StatusCode == http.StatusRequestEntityTooLarge
}

// exporter drains finished traces and posts OTLP/HTTP JSON in the background.
// It owns the only network path in the plugin.
type exporter struct {
	mu       sync.Mutex
	endpoint string
	headers  map[string]string
	timeout  time.Duration
	client   *http.Client

	// backoff tracks consecutive failures so a dead endpoint is retried with
	// increasing delay rather than hammered every flush interval.
	failures int
	lastErr  string
	nextTry  time.Time

	// queue holds spans whose export failed transiently. They are retried on a
	// later flush instead of being dropped, so a brief backend outage does not
	// lose the traces that happened during it.
	queue []otlpSpan

	sent    uint64
	dropped uint64
}

func newExporter() *exporter {
	return &exporter{headers: map[string]string{}}
}

func (e *exporter) configure(cfg config) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 2
	timeout := cfg.ExportTimeout
	if timeout <= 0 || timeout > maxExportTimeout {
		timeout = maxExportTimeout
	}
	client := &http.Client{Timeout: timeout, Transport: transport}

	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}

	e.mu.Lock()
	old := e.client
	e.endpoint = strings.TrimSpace(cfg.Endpoint)
	e.headers = headers
	e.timeout = timeout
	e.client = client
	e.failures = 0
	e.nextTry = time.Time{}
	e.mu.Unlock()

	if old != nil {
		old.CloseIdleConnections()
	}
}

func (e *exporter) ready(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.endpoint != "" && !now.Before(e.nextTry)
}

// requeue puts transiently-failed spans back for a later attempt. The queue is
// bounded: past the limit the oldest are dropped and counted, so an endpoint
// that stays down costs memory only up to the bound rather than without limit.
func (e *exporter) requeue(spans []otlpSpan) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queue = append(e.queue, spans...)
	if over := len(e.queue) - maxQueuedSpans; over > 0 {
		e.queue = append(e.queue[:0], e.queue[over:]...)
		e.dropped += uint64(over)
	}
}

// takeQueued returns and clears the retry queue.
func (e *exporter) takeQueued() []otlpSpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 {
		return nil
	}
	out := e.queue
	e.queue = nil
	return out
}

func (e *exporter) status() (sent, dropped uint64, lastErr string, failures int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sent, e.dropped, e.lastErr, e.failures
}

// send posts one OTLP batch. Empty endpoint means no export at all, which is
// the default so an unconfigured install sends nothing.
func (e *exporter) send(ctx context.Context, req otlpRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode otlp batch: %w", err)
	}
	return e.sendEncoded(ctx, body)
}

// sendEncoded posts an already-encoded OTLP batch. A caller that had to know
// the exact request size encodes once and hands the bytes here, so the batch is
// not marshalled twice.
func (e *exporter) sendEncoded(ctx context.Context, body []byte) error {
	e.mu.Lock()
	endpoint, headers, client := e.endpoint, e.headers, e.client
	e.mu.Unlock()

	if endpoint == "" {
		return nil
	}
	if client == nil {
		return errors.New("no http client")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// The endpoint can carry credentials in its userinfo; never echo it.
		return fmt.Errorf("build export request: %v", sanitizeURL(endpoint))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", pluginID+"/"+pluginVersion)
	for name, value := range headers {
		httpReq.Header.Set(name, value)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		e.recordFailure(sanitizeError(err))
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// A backend may return 200 and still reject individual spans; the body
	// carries partialSuccess when it does.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("http %d: %s", resp.StatusCode, truncateText(string(respBody), 400))
		// A 4xx (other than a rate limit) means this exact payload will always
		// be refused, so it is reported as permanent. 413 in particular is
		// recoverable by sending fewer spans, which the caller does.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			e.recordFailure(msg)
			return &permanentError{StatusCode: resp.StatusCode, Message: msg}
		}
		e.recordFailure(msg)
		return fmt.Errorf("%s", msg)
	}

	var partial struct {
		PartialSuccess *struct {
			RejectedSpans json.Number `json:"rejectedSpans"`
			ErrorMessage  string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if len(respBody) > 0 && json.Unmarshal(respBody, &partial) == nil && partial.PartialSuccess != nil {
		rejected, _ := partial.PartialSuccess.RejectedSpans.Int64()
		if rejected > 0 {
			// Not a transport failure: the request was accepted but some spans
			// were dropped by the backend. Record it so the health view shows it.
			e.recordPartial(rejected, partial.PartialSuccess.ErrorMessage)
		}
	}

	e.mu.Lock()
	e.failures = 0
	e.lastErr = ""
	e.sent++
	e.nextTry = time.Time{}
	e.mu.Unlock()
	return nil
}

func (e *exporter) recordFailure(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures++
	e.lastErr = msg
	backoff := baseBackoff << uint(min(e.failures, 6))
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	e.nextTry = time.Now().Add(backoff)
}

func (e *exporter) recordPartial(rejected int64, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastErr = fmt.Sprintf("backend rejected %d spans: %s", rejected, truncateText(message, 200))
}

// sanitizeError drops the URL from an error string, because the endpoint may
// carry credentials in its userinfo.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if idx := strings.Index(s, "://"); idx > 0 {
		if at := strings.Index(s[idx:], "@"); at > 0 {
			// Replace userinfo.
			prefix := s[:idx+3]
			rest := s[idx+3+at:]
			s = prefix + rest
		}
	}
	return truncateText(s, 400)
}

func sanitizeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "(invalid endpoint)"
	}
	parsed.User = nil
	return parsed.String()
}

// authHeaderValue builds a Basic header from a public/secret pair. Langfuse
// uses this shape; PostHog takes a plain bearer token.
func authHeaderValue(kind, user, secret string) string {
	switch strings.ToLower(kind) {
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
	default:
		return "Bearer " + secret
	}
}

func sha256Sum(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}
