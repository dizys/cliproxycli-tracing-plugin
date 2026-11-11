package main

import (
	"encoding/json"
	"strings"
)

// Delta extraction runs per chunk, so it is the one parse that cannot move off
// the request path. It is kept to a single json.Unmarshal of the chunk and a
// walk over a small map. Chunks that carry no text (role announcements, usage
// objects, keep-alives) are rejected with one comparison.

// extractDeltaText pulls the generated text out of one SSE chunk. The dialect
// is detected from the payload shape rather than from the provider name,
// because the host has already translated the response into the client's
// protocol, not the provider's.
func extractDeltaText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	payload := trimSSEFraming(body)
	if len(payload) == 0 {
		return ""
	}
	// Fast reject: no delta or text keyword means nothing to extract. This
	// avoids a JSON parse for role-only and usage-only chunks.
	if !hasAny(payload, "delta", "text", "content", "candidates", "output_text") {
		return ""
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return ""
	}

	// OpenAI chat/completions: choices[].delta.content
	if raw, ok := doc["choices"]; ok {
		if text := openAIDeltaText(raw); text != "" {
			return text
		}
	}
	// OpenAI Responses API: type=response.output_text.delta, delta=string
	if raw, ok := doc["delta"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	// Anthropic messages: type=content_block_delta, delta.text
	if raw, ok := doc["delta"]; ok {
		if text := anthropicDeltaText(raw); text != "" {
			return text
		}
	}
	// Gemini: candidates[].content.parts[].text
	if raw, ok := doc["candidates"]; ok {
		if text := geminiDeltaText(raw); text != "" {
			return text
		}
	}
	return ""
}

func openAIDeltaText(raw json.RawMessage) string {
	var choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &choices) != nil {
		return ""
	}
	var b strings.Builder
	for i := range choices {
		if choices[i].Delta.Content != "" {
			b.WriteString(choices[i].Delta.Content)
		} else if choices[i].Text != "" {
			// Legacy completions stream text directly.
			b.WriteString(choices[i].Text)
		}
	}
	return b.String()
}

func anthropicDeltaText(raw json.RawMessage) string {
	var delta struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &delta) != nil {
		return ""
	}
	return delta.Text
}

func geminiDeltaText(raw json.RawMessage) string {
	var candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &candidates) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range candidates {
		for _, p := range c.Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// extractCompletionText pulls the assistant text out of a complete
// non-streaming response body.
func extractCompletionText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	// OpenAI chat/completions: choices[].message.content
	if raw, ok := doc["choices"]; ok {
		var choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &choices) == nil {
			var b strings.Builder
			for _, c := range choices {
				if len(c.Message.Content) > 0 {
					b.WriteString(rawContentText(c.Message.Content))
				} else if c.Text != "" {
					b.WriteString(c.Text)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	// Anthropic messages: content[].text
	if raw, ok := doc["content"]; ok {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				b.WriteString(p.Text)
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	// Gemini: candidates[].content.parts[].text
	if raw, ok := doc["candidates"]; ok {
		return geminiDeltaText(raw)
	}
	// Responses API: output[].content[].text
	if raw, ok := doc["output"]; ok {
		var items []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &items) == nil {
			var b strings.Builder
			for _, it := range items {
				for _, c := range it.Content {
					b.WriteString(c.Text)
				}
			}
			return b.String()
		}
	}
	return ""
}

// rawContentText handles a message content field that may be a string or an
// array of typed parts.
func rawContentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// trimSSEFraming strips the "data: " prefix and blank-line framing the proxy
// leaves in a chunk, and rejects non-JSON control lines.
func trimSSEFraming(body []byte) []byte {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return nil
	}
	// A chunk may hold one or more "data: {...}" lines.
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		if strings.HasPrefix(line, "{") {
			return []byte(line)
		}
	}
	return nil
}

func hasAny(b []byte, needles ...string) bool {
	s := string(b)
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
