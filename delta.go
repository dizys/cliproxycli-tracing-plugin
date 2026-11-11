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
			Content   json.RawMessage `json:"content"`
			Text      string          `json:"text"`
			ToolCalls []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &choices) != nil {
		return ""
	}
	var b strings.Builder
	for i := range choices {
		c := &choices[i]
		if len(c.Delta.Content) > 0 {
			b.WriteString(rawContentText(c.Delta.Content))
		} else if c.Delta.Text != "" {
			// Legacy completions stream text directly.
			b.WriteString(c.Delta.Text)
		} else if c.Text != "" {
			b.WriteString(c.Text)
		}
		// Tool calls stream as a name on the first delta and argument
		// fragments after it. They are not assistant text, so they are marked
		// with a tag rather than concatenated into the answer.
		for _, tc := range c.Delta.ToolCalls {
			if tc.Function.Name != "" {
				b.WriteString("[tool_use] " + tc.Function.Name + " ")
			}
			if tc.Function.Arguments != "" {
				b.WriteString(tc.Function.Arguments)
			}
		}
	}
	return b.String()
}

// anthropicDeltaText handles the Anthropic stream event shapes. Beyond plain
// text deltas, an agent stream carries tool_use blocks (the model's arguments)
// and thinking blocks, both of which are part of the conversation.
func anthropicDeltaText(raw json.RawMessage) string {
	var delta map[string]json.RawMessage
	if json.Unmarshal(raw, &delta) != nil {
		return ""
	}
	var b strings.Builder

	var text string
	if json.Unmarshal(delta["text"], &text) == nil && text != "" {
		b.WriteString(text)
	}
	// input_json_delta carries tool arguments, one fragment at a time.
	if raw, ok := delta["partial_json"]; ok {
		var frag string
		if json.Unmarshal(raw, &frag) == nil {
			b.WriteString(frag)
		}
	}
	// thinking_delta carries reasoning text.
	if raw, ok := delta["thinking"]; ok {
		var think string
		if json.Unmarshal(raw, &think) == nil && think != "" {
			b.WriteString(think)
		}
	}
	return b.String()
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
	// OpenAI chat/completions: choices[].message, which may carry tool_calls
	// instead of, or alongside, content.
	if raw, ok := doc["choices"]; ok {
		var choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
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
				for _, tc := range c.Message.ToolCalls {
					if tc.Function.Name != "" {
						b.WriteString("[tool_use] " + tc.Function.Name + " ")
					}
					b.WriteString(tc.Function.Arguments)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	// Anthropic messages: content[] may hold text, tool_use, thinking blocks.
	if raw, ok := doc["content"]; ok {
		if text := rawContentText(raw); text != "" {
			return text
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
// array of typed parts, keeping tool calls and other non-text payloads.
func rawContentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteString("\n")
		}
		var asString string
		if json.Unmarshal(part, &asString) == nil {
			b.WriteString(asString)
			continue
		}
		var m map[string]any
		if json.Unmarshal(part, &m) == nil {
			b.WriteString(partToString(m))
			continue
		}
		b.WriteString(string(part))
	}
	return b.String()
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
