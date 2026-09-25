package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

// respWireTool is the Responses API function tool shape.
type respWireTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict,omitempty"`
}

// respBody is the Zen Responses wire shape (subset).
type respBody struct {
	Model           string         `json:"model"`
	Input           any            `json:"input"`
	Stream          bool           `json:"stream"`
	Tools           []respWireTool `json:"tools,omitempty"`
	Reasoning       any            `json:"reasoning,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
}

// skeletalRespTool is the minimal entry the gate requires (names matter).
func skeletalRespTool(name string) respWireTool {
	return respWireTool{
		Type: "function", Name: name, Description: "d",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// ensureRespGateTools appends skeletal bash/read entries when absent.
func ensureRespGateTools(tools []respWireTool) []respWireTool {
	var bash, read bool
	for _, t := range tools {
		switch t.Name {
		case "bash":
			bash = true
		case "read":
			read = true
		}
	}
	if !bash {
		tools = append(tools, skeletalRespTool("bash"))
	}
	if !read {
		tools = append(tools, skeletalRespTool("read"))
	}
	return tools
}

// buildRespBody maps a normalized request to the Zen wire shape.
func buildRespBody(req provider.ResponsesRequest, model string) respBody {
	tools := make([]respWireTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, respWireTool{
			Type: "function", Name: t.Name,
			Description: t.Description, Parameters: t.Parameters,
		})
	}
	return respBody{
		Model: model, Input: req.Input, Stream: true,
		Tools:     ensureRespGateTools(tools),
		Reasoning: req.Reasoning, MaxOutputTokens: req.MaxOutputTokens,
	}
}

func newRespID() (string, error) {
	const b62 = b62Chars
	raw, err := randStr(b62, 26)
	if err != nil {
		return "", err
	}
	return "resp_" + raw, nil
}

// postResponsesRaw sends the body with the genuine recipe and returns the
// raw SSE stream on 200. No retries: a 4xx is a decision, quota is precious.
func (c *zenChatClient) postResponsesRaw(ctx context.Context, body respBody) (io.ReadCloser, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode responses: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.recipe.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("responses request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.recipe.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.recipe.UserAgent)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-opencode-client", c.recipe.Client)
	httpReq.Header.Set("x-opencode-project", c.recipe.Project)
	reqID, err := newRequestID()
	if err != nil {
		return nil, err
	}
	sesID, err := newSessionID()
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-opencode-request", reqID)
	httpReq.Header.Set("x-opencode-session", sesID)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("responses fetch: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, parseUpstreamError(resp)
	}
	return resp.Body, nil
}

// aggregateResponses folds the SSE stream until response.completed and
// returns that event's response object with the envelope rewritten.
// Output items pass through byte-identical.
func aggregateResponses(r io.Reader, id, model string, createdAt int64) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil || ev.Type != "response.completed" {
			continue
		}
		var full map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &full) != nil {
			continue
		}
		respRaw, ok := full["response"]
		if !ok {
			continue
		}
		var resp map[string]json.RawMessage
		if json.Unmarshal(respRaw, &resp) != nil {
			continue
		}
		resp["id"], _ = json.Marshal(id)
		resp["model"], _ = json.Marshal(model)
		resp["created_at"], _ = json.Marshal(createdAt)
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("encode response: %w", err)
		}
		return out, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read sse: %w", err)
	}
	return nil, fmt.Errorf("responses: completed event not found")
}
