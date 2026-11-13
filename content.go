package main

import (
	"encoding/json"
	"strings"
)

// Content is carried in the OTel GenAI "parts" message shape, which is what
// PostHog renders and what its tool-call extractor reads:
//
//	{"role":"assistant","parts":[
//	  {"type":"text","content":"Let me look."},
//	  {"type":"tool_call","id":"toolu_1","name":"read_file","arguments":{...}}]}
//
// The flat `{"role":..,"content":"..."}` shape is deliberately not used. A flat
// string renders as prose but is invisible to the extractor, so tool calls
// disappear from PostHog's Tools view and their arguments become unparseable
// text. One representation serves both rendering and extraction.
//
// PostHog's ingestion reads these part types and turns them into its own view:
// `text` and `reasoning` render inline, `tool_call` becomes a tool call with
// its arguments, and `tool_call_response` becomes a correlated `role: "tool"`
// follow-up message.

// part is one content part in the OTel GenAI message schema. Arguments and
// Response stay raw so a provider's object is passed through unchanged rather
// than re-encoded as a string.
type part struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
}

// chatMessage is one turn. PostHog requires a string role alongside a parts
// list to treat the message as semconv content.
type chatMessage struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

const (
	partText            = "text"
	partReasoning       = "reasoning"
	partToolCall        = "tool_call"
	partToolCallResp    = "tool_call_response"
	partServerToolCall  = "server_tool_call"
	partServerToolResp  = "server_tool_call_response"
	partBlob            = "blob"
	partURI             = "uri"
	partFile            = "file"
	partCompaction      = "compaction"
	partToolApprovalOK  = "tool_approval_response"
	partCustomUnknown   = "custom"
	omitEarlierMessages = "...[earlier messages omitted]\n"
)

// contentParts converts a provider `content` field, which is either a string or
// an array of typed blocks, into semconv parts.
//
// The block shapes differ by provider dialect and all of them are in the wild
// for agent traffic: Anthropic uses `text`/`tool_use`/`tool_result`/`thinking`,
// OpenAI uses `text`/`image_url`, and Gemini uses `text`/`inline_data`. Anything
// unrecognised keeps its payload as text rather than being dropped, so a new
// block type degrades to visible content instead of vanishing.
func contentParts(v any) []part {
	switch typed := v.(type) {
	case nil:
		return nil
	case string:
		if typed == "" {
			return nil
		}
		return []part{{Type: partText, Content: typed}}
	case []any:
		out := make([]part, 0, len(typed))
		for _, raw := range typed {
			if p, ok := blockToPart(raw); ok {
				out = append(out, p)
			}
		}
		return out
	default:
		// A bare object or scalar in a content slot: keep it visible.
		if raw, err := json.Marshal(typed); err == nil && len(raw) > 0 && string(raw) != "null" {
			return []part{{Type: partText, Content: string(raw)}}
		}
		return nil
	}
}

// blockToPart maps one provider content block onto a semconv part.
func blockToPart(raw any) (part, bool) {
	s, ok := raw.(string)
	if ok {
		if s == "" {
			return part{}, false
		}
		return part{Type: partText, Content: s}, true
	}
	m, ok := raw.(map[string]any)
	if !ok {
		if enc, err := json.Marshal(raw); err == nil && len(enc) > 0 && string(enc) != "null" {
			return part{Type: partText, Content: string(enc)}, true
		}
		return part{}, false
	}

	typ := stringField(m, "type")
	switch typ {
	case partText, "":
		// Anthropic uses `text`; the semconv and some producers use `content`.
		if text := firstStringField(m, "text", "content"); text != "" {
			return part{Type: partText, Content: text}, true
		}
		if typ == "" {
			return unknownBlockPart(m, "")
		}
		return part{}, false

	case "tool_use", partToolCall, "function_call":
		return toolCallPart(m, typ), true

	case "tool_result", partToolCallResp:
		return toolResultPart(m, typ), true

	case "thinking", partReasoning:
		// Anthropic reasoning text is `thinking`; semconv uses `content`.
		if text := firstStringField(m, "thinking", "content"); text != "" {
			return part{Type: partReasoning, Content: text}, true
		}
		return part{}, false

	case "server_tool_use", partServerToolCall:
		return toolCallPart(m, partServerToolCall), true

	case partServerToolResp:
		return toolResultPart(m, partServerToolResp), true

	case "image", "image_url", partBlob, partURI, partFile:
		return mediaPart(m, typ), true

	case "redacted_thinking":
		// Anthropic encrypts some reasoning blocks. The ciphertext is not
		// readable, so it is kept as a labelled marker rather than dropped.
		if data := firstStringField(m, "data"); data != "" {
			return part{Type: partText, Content: "[redacted thinking] " + data}, true
		}
		return part{Type: partText, Content: "[redacted thinking]"}, true
	}

	return unknownBlockPart(m, typ)
}

// unknownBlockPart keeps an unrecognised block visible. It is emitted as text
// so it renders, with the block type as a readable prefix and the full payload
// preserved, rather than becoming an invisible part type.
func unknownBlockPart(m map[string]any, typ string) (part, bool) {
	payload := make(map[string]any, len(m))
	for k, v := range m {
		if k == "type" {
			continue
		}
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) == 0 {
		if typ == "" {
			return part{}, false
		}
		return part{Type: partText, Content: "[" + typ + "]"}, true
	}
	if typ == "" {
		return part{Type: partText, Content: string(raw)}, true
	}
	return part{Type: partText, Content: "[" + typ + "] " + string(raw)}, true
}

func toolCallPart(m map[string]any, typ string) part {
	p := part{
		Type: partToolCall,
		ID:   firstStringField(m, "id", "tool_use_id", "call_id", "tool_call_id"),
		Name: firstStringField(m, "name", "tool_name"),
	}
	if typ == partServerToolCall || typ == "server_tool_use" {
		p.Type = partServerToolCall
	}

	// OpenAI nests the call under `function`: {id, type, function:{name,
	// arguments}}. Anthropic puts `name` and `input` at the top level, and the
	// Responses API puts them on the item. All three are accepted.
	nested, hasNested := m["function"].(map[string]any)
	if hasNested {
		if p.Name == "" {
			p.Name = firstStringField(nested, "name")
		}
		if raw, ok := nested["arguments"]; ok && raw != nil {
			p.Arguments = marshalRaw(raw)
		}
	}

	// Arguments arrive as `input` (Anthropic), `arguments` (OpenAI), or the
	// polymorphic server_tool_call payload. A top-level value wins only when the
	// nested form did not supply one.
	if len(p.Arguments) == 0 {
		for _, key := range []string{"input", "arguments", partServerToolCall} {
			if raw, ok := m[key]; ok && raw != nil {
				p.Arguments = marshalRaw(raw)
				break
			}
		}
	}
	return p
}

func toolResultPart(m map[string]any, typ string) part {
	p := part{
		Type: partToolCallResp,
		ID:   firstStringField(m, "tool_use_id", "id", "call_id", "tool_call_id"),
	}
	// A result body is `content` (Anthropic) or `output` (Responses API), and
	// the semconv name for it is `response`.
	var raw any
	for _, key := range []string{"content", "response", "output", "result"} {
		if v, ok := m[key]; ok && v != nil {
			raw = v
			break
		}
	}
	if raw == nil {
		return p
	}
	// A result may itself be an array of typed blocks. Flattening it to text
	// keeps the result readable instead of nesting a parts array inside a part.
	if blocks, ok := raw.([]any); ok {
		var b strings.Builder
		for i, block := range blocks {
			if i > 0 {
				b.WriteString("\n")
			}
			if sub, okSub := blockToPart(block); okSub {
				b.WriteString(partToText(sub))
			}
		}
		p.Response = marshalRaw(b.String())
		return p
	}
	p.Response = marshalRaw(raw)
	if typ == partServerToolResp {
		p.Type = partServerToolResp
	}
	return p
}

// mediaPart keeps an image or file reference. The payload is not inlined as
// base64 here: it is already base64 in the request, and re-encoding it as a
// blob part would double a large prompt. A short marker plus the reference is
// what a consumer can still act on.
func mediaPart(m map[string]any, typ string) part {
	if url := mediaURL(m); url != "" {
		return part{Type: partURI, Content: url}
	}
	return part{Type: partText, Content: "[" + typ + "]"}
}

func mediaURL(m map[string]any) string {
	if s := firstStringField(m, "url", "uri", "file_id"); s != "" {
		return s
	}
	if nested, ok := m["image_url"].(map[string]any); ok {
		if s := stringField(nested, "url"); s != "" {
			return s
		}
	}
	if nested, ok := m["source"].(map[string]any); ok {
		if s := firstStringField(nested, "url", "data"); s != "" {
			return s
		}
	}
	return ""
}

// partToText renders a part as plain text, used when a part has to be folded
// into a string (a nested tool result).
func partToText(p part) string {
	switch p.Type {
	case partText, partReasoning:
		return p.Content
	case partToolCall, partServerToolCall:
		if len(p.Arguments) > 0 {
			return p.Name + " " + string(p.Arguments)
		}
		return p.Name
	case partToolCallResp, partServerToolResp:
		if len(p.Response) > 0 {
			var s string
			if json.Unmarshal(p.Response, &s) == nil {
				return s
			}
			return string(p.Response)
		}
		return ""
	default:
		return p.Content
	}
}

func marshalRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// promptMessages turns the stored client body into parts-shaped messages. This
// runs on the exporter goroutine, never on the request path.
//
// The body shape differs by protocol: OpenAI chat uses `messages` with
// assistant `tool_calls` at message level, the Responses API uses `input`, and
// Anthropic uses `messages` plus a separate `system` string. All are accepted;
// anything unrecognised is kept as one user message so no content is lost.
func promptMessages(body []byte) []chatMessage {
	if len(body) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return []chatMessage{{Role: "user", Parts: []part{{Type: partText, Content: string(body)}}}}
	}

	var out []chatMessage
	if system := stringField(doc, "system"); system != "" {
		out = append(out, chatMessage{Role: "system", Parts: []part{{Type: partText, Content: system}}})
	}
	// Gemini carries the system prompt under a different key.
	if system := stringField(doc, "system_instruction"); system != "" && len(out) == 0 {
		out = append(out, chatMessage{Role: "system", Parts: []part{{Type: partText, Content: system}}})
	}

	if msgs, ok := doc["messages"].([]any); ok {
		for _, item := range msgs {
			if m, ok := item.(map[string]any); ok {
				if msg, okMsg := messageFromMap(m); okMsg {
					out = append(out, msg)
				}
			}
		}
	}
	if len(out) == 0 {
		switch input := doc["input"].(type) {
		case string:
			if input != "" {
				out = append(out, chatMessage{Role: "user", Parts: []part{{Type: partText, Content: input}}})
			}
		case []any:
			for _, item := range input {
				if m, ok := item.(map[string]any); ok {
					if msg, okMsg := messageFromMap(m); okMsg {
						out = append(out, msg)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		if prompt := stringField(doc, "prompt"); prompt != "" {
			out = append(out, chatMessage{Role: "user", Parts: []part{{Type: partText, Content: prompt}}})
		}
	}
	if len(out) == 0 {
		out = append(out, chatMessage{Role: "user", Parts: []part{{Type: partText, Content: string(body)}}})
	}
	for i := range out {
		if out[i].Role == "" {
			out[i].Role = "user"
		}
	}
	return out
}

// messageFromMap builds one message, lifting OpenAI's message-level tool_calls
// into parts and mapping a `role: "tool"` result to a correlated response part.
func messageFromMap(m map[string]any) (chatMessage, bool) {
	role := stringField(m, "role")
	parts := contentParts(m["content"])

	// OpenAI chat: the assistant's tool calls live beside `content`, and the
	// content is often null when the turn is nothing but a tool call.
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, raw := range calls {
			if call, okCall := raw.(map[string]any); okCall {
				parts = append(parts, toolCallPart(call, partToolCall))
			}
		}
	}

	// A tool result sent as its own turn carries the id at message level.
	if role == "tool" {
		id := firstStringField(m, "tool_call_id", "tool_use_id")
		resp := part{Type: partToolCallResp, ID: id}
		if out, okOut := m["content"]; okOut {
			resp.Response = marshalRaw(out)
		}
		parts = []part{resp}
	}

	if len(parts) == 0 && role == "" {
		return chatMessage{}, false
	}
	return chatMessage{Role: role, Parts: parts}, true
}

// completionParts turns a completed non-streaming response body into the
// assistant's parts.
func completionParts(body []byte) []part {
	if len(body) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return []part{{Type: partText, Content: string(body)}}
	}

	// OpenAI chat: choices[].message, with tool_calls beside content.
	if raw, ok := doc["choices"].([]any); ok {
		var out []part
		for _, item := range raw {
			choice, ok := item.(map[string]any)
			if !ok {
				continue
			}
			msg, okMsg := choice["message"].(map[string]any)
			if !okMsg {
				// Legacy completions put text directly on the choice.
				if text := stringField(choice, "text"); text != "" {
					out = append(out, part{Type: partText, Content: text})
				}
				continue
			}
			out = append(out, messageParts(msg)...)
		}
		if len(out) > 0 {
			return out
		}
	}
	// Anthropic: content[] holds text, tool_use and thinking blocks.
	if raw, ok := doc["content"].([]any); ok {
		if parts := contentParts(raw); len(parts) > 0 {
			return parts
		}
	}
	// Gemini: candidates[].content.parts[].
	if raw, ok := doc["candidates"].([]any); ok {
		var out []part
		for _, item := range raw {
			choice, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if c, okC := choice["content"].(map[string]any); okC {
				out = append(out, contentParts(c["parts"])...)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Responses API: output[] items, which are either messages with content or
	// bare function_call items.
	if raw, ok := doc["output"].([]any); ok {
		var out []part
		for _, item := range raw {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringField(entry, "type") == "function_call" {
				out = append(out, toolCallPart(entry, partToolCall))
				continue
			}
			if c, okC := entry["content"].([]any); okC {
				out = append(out, contentParts(c)...)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// messageParts lifts one OpenAI-shaped message into parts.
func messageParts(m map[string]any) []part {
	parts := contentParts(m["content"])
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, raw := range calls {
			if call, okCall := raw.(map[string]any); okCall {
				parts = append(parts, toolCallPart(call, partToolCall))
			}
		}
	}
	return parts
}

// toolDefinitions returns the declared tool catalog from a request body, in the
// shape PostHog maps to `$ai_tools`. Both the OpenAI and Anthropic encodings are
// passed through unchanged because the consumer accepts either.
func toolDefinitions(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	tools, ok := doc["tools"].([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	return marshalRaw(tools)
}

// messagesJSON encodes the messages for a span attribute.
func messagesJSON(msgs []chatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	return string(raw)
}
