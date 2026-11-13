package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// Delta extraction runs per chunk, so it is the one parse that cannot move off
// the request path. It stays at one json.Unmarshal per chunk plus an append;
// chunks with no content (role announcements, usage objects, keep-alives) are
// rejected by a keyword scan before any parse.
//
// A streamed response arrives as fragments with no content array: text arrives
// as `delta.content`, Anthropic reasoning as `delta.thinking`, and a tool call
// as a name on one chunk followed by argument fragments on later ones, keyed by
// index. Reassembling that into the same parts shape a non-streaming response
// produces is what makes a streamed tool call visible as a tool call at all.

// streamDelta is one chunk's contribution, already split by kind.
type streamDelta struct {
	Text      string
	Reasoning string
	ToolCalls []toolCallDelta
}

// toolCallDelta is one fragment of a streamed tool call. HasIndex distinguishes
// "index 0" from "no index at all", which is what separates an OpenAI fragment
// (always indexed, so parallel calls interleave) from an Anthropic argument
// fragment (never indexed, attaches to the open call).
type toolCallDelta struct {
	Index     int
	HasIndex  bool
	ID        string
	Name      string
	Arguments string
}

func (d streamDelta) empty() bool {
	return d.Text == "" && d.Reasoning == "" && len(d.ToolCalls) == 0
}

// extractDelta pulls the content out of one SSE chunk. The dialect is detected
// from the payload shape rather than from the provider name, because the host
// has already translated the response into the client's protocol.
func extractDelta(body []byte) streamDelta {
	if len(body) == 0 {
		return streamDelta{}
	}
	payload := trimSSEFraming(body)
	if len(payload) == 0 {
		return streamDelta{}
	}
	// Fast reject: something in this chunk must indicate content.
	if !hasAny(payload, "delta", "text", "content", "candidates", "output_text", "tool_call", "partial_json", "thinking", "content_block_start") {
		return streamDelta{}
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return streamDelta{}
	}

	// Anthropic announces a block before streaming into it, and that
	// announcement is the only place a streamed tool call's name and id appear:
	// the argument fragments that follow carry neither. Without this branch a
	// streamed tool call would be unnamed and PostHog's extractor would skip it.
	if raw, ok := doc["content_block"]; ok {
		if d := anthropicBlockStart(raw); !d.empty() {
			return d
		}
	}

	// OpenAI chat/completions: choices[].delta
	if raw, ok := doc["choices"]; ok {
		if d := openAIDelta(raw); !d.empty() {
			return d
		}
	}
	// OpenAI Responses API: type=response.output_text.delta, delta is a string.
	if raw, ok := doc["delta"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return streamDelta{Text: s}
		}
		// Anthropic messages: delta.text / thinking / partial_json.
		if d := anthropicDelta(raw); !d.empty() {
			return d
		}
	}
	// Gemini: candidates[].content.parts[].text
	if raw, ok := doc["candidates"]; ok {
		if text := geminiDelta(raw); text != "" {
			return streamDelta{Text: text}
		}
	}
	return streamDelta{}
}

// anthropicBlockStart reads a content_block_start event, which announces the
// block whose fragments follow. A tool_use block carries the name and id here
// and nowhere else, so this is what names a streamed tool call.
func anthropicBlockStart(raw json.RawMessage) streamDelta {
	var block struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return streamDelta{}
	}
	switch block.Type {
	case "tool_use", "server_tool_use":
		d := toolCallDelta{ID: block.ID, Name: block.Name}
		// A non-empty `input` here means the whole call arrived at once.
		if len(block.Input) > 0 && string(block.Input) != "{}" {
			d.Arguments = string(block.Input)
		}
		return streamDelta{ToolCalls: []toolCallDelta{d}}
	}
	// Text, thinking and other blocks start empty and carry their content in
	// the deltas that follow.
	return streamDelta{}
}

func openAIDelta(raw json.RawMessage) streamDelta {
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
		return streamDelta{}
	}
	var d streamDelta
	var text strings.Builder
	for i := range choices {
		c := &choices[i]
		if len(c.Delta.Content) > 0 {
			var s string
			if json.Unmarshal(c.Delta.Content, &s) == nil {
				text.WriteString(s)
			} else {
				// Content may be a parts array on some producers.
				var parts []any
				if json.Unmarshal(c.Delta.Content, &parts) == nil {
					for _, p := range contentParts(parts) {
						text.WriteString(partToText(p))
					}
				}
			}
		} else if c.Delta.Text != "" {
			text.WriteString(c.Delta.Text)
		} else if c.Text != "" {
			text.WriteString(c.Text)
		}
		for _, tc := range c.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			d.ToolCalls = append(d.ToolCalls, toolCallDelta{
				Index:     idx,
				HasIndex:  tc.Index != nil,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	d.Text = text.String()
	return d
}

// anthropicDelta handles Anthropic stream event shapes beyond plain text: tool
// input fragments and reasoning.
func anthropicDelta(raw json.RawMessage) streamDelta {
	var delta map[string]json.RawMessage
	if json.Unmarshal(raw, &delta) != nil {
		return streamDelta{}
	}
	var d streamDelta
	if raw, ok := delta["text"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			d.Text = s
		}
	}
	// thinking_delta carries reasoning text.
	if raw, ok := delta["thinking"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			d.Reasoning = s
		}
	}
	// input_json_delta carries tool arguments, one fragment at a time.
	if raw, ok := delta["partial_json"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			d.ToolCalls = append(d.ToolCalls, toolCallDelta{Arguments: s})
		}
	}
	return d
}

func geminiDelta(raw json.RawMessage) string {
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

// streamParts assembles the accumulated stream state into semconv parts.
//
// Parts are ordered as the model produced them: reasoning, then text, then tool
// calls. A tool call is emitted once, with its argument fragments joined, and
// carries whatever name and id the stream supplied.
func streamParts(text, reasoning string, tools []streamTool) []part {
	var out []part
	if reasoning != "" {
		out = append(out, part{Type: partReasoning, Content: reasoning})
	}
	if text != "" {
		out = append(out, part{Type: partText, Content: text})
	}
	ordered := make([]streamTool, len(tools))
	copy(ordered, tools)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	for _, t := range ordered {
		p := part{Type: partToolCall, ID: t.ID, Name: t.Name}
		args := strings.TrimSpace(t.Arguments)
		if args != "" {
			// Arguments stream in as fragments and are only valid JSON once
			// complete. Anything that is not valid JSON is kept as a string so
			// a truncated stream still shows what the model produced.
			if json.Valid([]byte(args)) {
				p.Arguments = json.RawMessage(args)
			} else {
				p.Arguments = marshalRaw(args)
			}
		}
		out = append(out, p)
	}
	return out
}

// trimSSEFraming strips the "data: " prefix and blank-line framing the proxy
// leaves in a chunk, and rejects non-JSON control lines.
func trimSSEFraming(body []byte) []byte {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return nil
	}
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
