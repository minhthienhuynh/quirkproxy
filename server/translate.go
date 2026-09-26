package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// chatToResponses maps a normalized chat request to a Responses request.
// Multi-turn history keeps its structure as Responses input items so the
// agentic tool loop survives the fallback: assistant text -> message item,
// assistant tool_calls -> function_call items, role:"tool" results ->
// function_call_output items, turn order preserved. A lone text message maps
// to a plain input string (back-compat). tool_choice/temperature are dropped:
// the Responses wire has no equivalent. Callers guarantee at least one
// message (the chat handler rejects empty requests with 400).
func chatToResponses(req provider.ChatRequest) provider.ResponsesRequest {
	var input any
	if len(req.Messages) == 1 && req.Messages[0].Role == "user" &&
		len(req.Messages[0].ToolCalls) == 0 && req.Messages[0].ToolCallID == "" {
		input = req.Messages[0].Content
	} else {
		items := make([]any, 0, len(req.Messages))
		for _, m := range req.Messages {
			items = append(items, chatMessageToInputItems(m)...)
		}
		input = items
	}
	out := provider.ResponsesRequest{Model: req.Model, Input: input}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, provider.ResponsesTool{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		})
	}
	out.MaxOutputTokens = req.MaxTokens
	return out
}

// chatMessageToInputItems expands one chat message into Responses input items,
// matching the genuine client shape (no explicit "message" type; user content
// is an input_text array while assistant/system content is a plain string):
// a tool result becomes a function_call_output, an assistant turn becomes a
// message item plus one function_call per call (call_id only — never an item
// id), and any other text becomes a message item.
func chatMessageToInputItems(m provider.ChatMessage) []any {
	if m.ToolCallID != "" && len(m.ToolCalls) == 0 {
		return []any{map[string]any{
			"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content,
		}}
	}
	var items []any
	if m.Content != "" {
		role := m.Role
		if role == "" {
			role = "user"
		}
		var content any = m.Content
		if role == "user" {
			content = []any{map[string]any{"type": "input_text", "text": m.Content}}
		}
		items = append(items, map[string]any{"role": role, "content": content})
	}
	for _, tc := range m.ToolCalls {
		items = append(items, map[string]any{
			"type": "function_call", "call_id": tc.ID,
			"name": tc.Name, "arguments": tc.Arguments,
		})
	}
	return items
}

// responsesToChat maps a normalized Responses request to a chat request.
// String input becomes one user message; array input extracts role/text.
// reasoning is dropped: the chat wire has no equivalent.
func responsesToChat(req provider.ResponsesRequest) provider.ChatRequest {
	out := provider.ChatRequest{Model: req.Model}
	switch in := req.Input.(type) {
	case nil:
		break
	case string:
		out.Messages = []provider.ChatMessage{{Role: "user", Content: in}}
	default:
		if items, ok := toInputItems(req.Input); ok {
			for _, it := range items {
				if role, text := parseInputItem(it); text != "" || role != "" {
					if role == "" {
						role = "user"
					}
					out.Messages = append(out.Messages, provider.ChatMessage{Role: role, Content: text})
				}
			}
		} else {
			out.Messages = []provider.ChatMessage{{Role: "user", Content: fmt.Sprint(in)}}
		}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, provider.ChatTool{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		})
	}
	out.MaxTokens = req.MaxOutputTokens
	return out
}

func toInputItems(input any) ([]any, bool) {
	if items, ok := input.([]any); ok {
		return items, true
	}
	return nil, false
}

// parseInputItem extracts role/text from one Responses input item.
func parseInputItem(item any) (string, string) {
	m, ok := item.(map[string]any)
	if !ok {
		if s, ok := item.(string); ok {
			return "user", s
		}
		return "", ""
	}
	role, _ := m["role"].(string)
	var text string
	switch c := m["content"].(type) {
	case string:
		text = c
	case []any:
		parts := make([]string, 0, len(c))
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["text"].(string); t != "" {
				parts = append(parts, t)
				continue
			}
			if t, _ := pm["input_text"].(string); t != "" {
				parts = append(parts, t)
				continue
			}
			if t, _ := pm["output_text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		text = strings.Join(parts, "\n")
	}
	if role == "" {
		role = "user"
	}
	return role, text
}

// responsesPayloadToChatResult folds a terminal Responses payload into a
// chat result: message items concatenate output_text, function_call items
// become tool calls, reasoning items are dropped. status "incomplete"
// (truncation) keeps its partial output and reports the cut through
// FinishReason — including an empty result when the cut landed before any
// usable output (reasoning-only). status "failed" is an error carrying the
// upstream message. A non-truncated payload folding to nothing usable (no
// message and no function_call) is an error so the handler surfaces 502
// instead of rendering an empty 200.
func responsesPayloadToChatResult(payload []byte) (provider.ChatResult, error) {
	var resp struct {
		Output            []json.RawMessage `json:"output"`
		Status            string            `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return provider.ChatResult{}, err
	}
	if resp.Status == "failed" {
		msg := resp.Error.Message
		if msg == "" {
			msg = "upstream response failed"
		}
		return provider.ChatResult{}, &provider.UpstreamError{
			Status: 502, Type: "UpstreamResponseFailed", Message: msg,
		}
	}
	var out provider.ChatResult
	sawUsable := false
	for _, raw := range resp.Output {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			continue
		}
		switch header.Type {
		case "message":
			var msg struct {
				Content []struct {
					Type       string `json:"type"`
					Text       string `json:"text"`
					InputText  string `json:"input_text"`
					OutputText string `json:"output_text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			sawUsable = true
			for _, c := range msg.Content {
				t := c.Text
				if t == "" {
					t = c.InputText
				}
				if t == "" {
					t = c.OutputText
				}
				if t == "" {
					continue
				}
				if out.Content != "" {
					out.Content += "\n"
				}
				out.Content += t
			}
		case "function_call":
			var call struct {
				CallID    string `json:"call_id"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(raw, &call); err != nil {
				continue
			}
			sawUsable = true
			id := call.CallID
			if id == "" {
				id = call.ID
			}
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{ID: id, Name: call.Name, Arguments: call.Arguments})
		}
	}
	if resp.Status == "incomplete" {
		out.FinishReason = "length"
		if resp.IncompleteDetails.Reason == "content_filter" {
			out.FinishReason = "content_filter"
		}
	}
	if !sawUsable && resp.Status != "incomplete" {
		return provider.ChatResult{}, fmt.Errorf("responses: no message or function_call in output")
	}
	return out, nil
}

// chatResultToResponsesPayload builds a completed Responses payload from a
// chat result with the envelope set to id/model/created.
func chatResultToResponsesPayload(res provider.ChatResult, model, id string, created int64) []byte {
	output := []any{
		map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": res.Content}},
		},
	}
	for _, tc := range res.ToolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "id": tc.ID, "call_id": tc.ID,
			"name": tc.Name, "arguments": tc.Arguments,
		})
	}
	out, _ := json.Marshal(map[string]any{
		"id":     id,
		"object": "response", "model": model, "created_at": created,
		"status": "completed", "output": output,
	})
	return out
}

// synthesizeChatSSE renders one full chat chunk plus a single [DONE] for the
// buffered-fallback stream path.
func synthesizeChatSSE(res provider.ChatResult, model, id string, created int64) string {
	delta := map[string]any{"role": "assistant", "content": res.Content}
	finish := chatFinishReason(res)
	if len(res.ToolCalls) > 0 {
		tcs := make([]any, 0, len(res.ToolCalls))
		for i, tc := range res.ToolCalls {
			tcs = append(tcs, map[string]any{
				"index": i, "id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			})
		}
		delta["tool_calls"] = tcs
	}
	chunk, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	var sb strings.Builder
	sb.WriteString("data: " + string(chunk) + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// synthesizeResponsesSSE renders created + completed events for the
// buffered-fallback stream path, rewriting the envelope.
func synthesizeResponsesSSE(payload []byte, model, id string, created int64) string {
	var resp map[string]any
	if err := json.Unmarshal(payload, &resp); err != nil {
		resp = map[string]any{}
	}
	resp["id"] = id
	resp["model"] = model
	resp["created_at"] = created
	full, _ := json.Marshal(resp)
	createdResp, _ := json.Marshal(map[string]any{
		"id": id, "model": model, "created_at": created, "status": "in_progress", "output": []any{},
	})
	createdEv, _ := json.Marshal(map[string]any{"type": "response.created", "response": json.RawMessage(createdResp)})
	completedEv, _ := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(full)})
	var sb strings.Builder
	sb.WriteString("event: response.created\n")
	sb.WriteString("data: " + string(createdEv) + "\n\n")
	sb.WriteString("event: response.completed\n")
	sb.WriteString("data: " + string(completedEv) + "\n\n")
	return sb.String()
}
