package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/minhthienhuynh/quirkproxy/provider"
)

var respIDPat = regexp.MustCompile(`^resp_[A-Za-z0-9]{26}$`)

func TestResponsesRecipeHeaders(t *testing.T) {
	var gotUA, gotClient, gotProject, gotReq, gotSes, gotAuth, gotCT, gotAccept string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotClient = r.Header.Get("x-opencode-client")
		gotProject = r.Header.Get("x-opencode-project")
		gotReq = r.Header.Get("x-opencode-request")
		gotSes = r.Header.Get("x-opencode-session")
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"object\":\"response\",\"model\":\"m\",\"created_at\":1,\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer srv.Close()

	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(map[string]any{
		"zen_api_key": "test-key",
	}, srv.URL)}
	raw, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{
		Model: "muse-spark-1.3-contributor-free",
		Input: "hi",
	}, "muse-spark-1.3-contributor-free"))
	if err != nil {
		t.Fatalf("postResponsesRaw: %v", err)
	}
	defer raw.Close()
	if gotUA != defaultUserAgent {
		t.Fatalf("UA = %q, want pinned %q", gotUA, defaultUserAgent)
	}
	if gotClient != "cli" || gotProject != "global" {
		t.Fatalf("client/project = %q/%q", gotClient, gotProject)
	}
	if !msgPat.MatchString(gotReq) || !sesPat.MatchString(gotSes) {
		t.Fatalf("id formats: req=%q ses=%q", gotReq, gotSes)
	}
	if gotAuth != "Bearer test-key" || gotCT != "application/json" {
		t.Fatalf("auth/ct = %q/%q", gotAuth, gotCT)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("accept = %q", gotAccept)
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true (gate requirement)", gotBody["stream"])
	}
	names := []string{}
	for _, tool := range gotBody["tools"].([]any) {
		m := tool.(map[string]any)
		names = append(names, m["name"].(string))
	}
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("bash") || !has("read") || !has("glob") || !has("grep") {
		t.Fatalf("tools = %v, want bash+read+glob+grep ensured", names)
	}
}

func TestResponsesFieldPassthrough(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"model\":\"m\",\"created_at\":1,\"output\":[]}}\n\n"))
	}))
	defer srv.Close()

	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	reasoning := map[string]any{"effort": "high"}
	maxTok := 256
	raw, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{
		Model:           "m",
		Input:           []any{map[string]any{"role": "user", "content": "hi"}},
		Reasoning:       reasoning,
		MaxOutputTokens: &maxTok,
	}, "m"))
	if err != nil {
		t.Fatalf("postResponsesRaw: %v", err)
	}
	defer raw.Close()
	if gotBody["input"] == nil {
		t.Fatal("input not passed through")
	}
	gotReasoning, _ := gotBody["reasoning"].(map[string]any)
	if gotReasoning["effort"] != "high" {
		t.Fatalf("reasoning = %v, want effort=high", gotBody["reasoning"])
	}
	if gotBody["max_output_tokens"].(float64) != 256 {
		t.Fatalf("max_output_tokens = %v, want 256", gotBody["max_output_tokens"])
	}
}

func TestResponsesAggregation(t *testing.T) {
	enc := "Q-PaDgEM2bGoLQypOLRx8onmqs_yOOCZHD_o4F2kKw8gP2VZPgWveH_0f_O4oEmww"
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_up\",\"model\":\"m\",\"created_at\":1,\"output\":[]}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_1:rs_2\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1:rs_2\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"" + enc + "\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"model\":\"m\",\"created_at\":1,\"status\":\"completed\",\"output\":[{\"id\":\"rs_1:rs_2\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"" + enc + "\"},{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	raw, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{
		Model: "m", Input: "hi",
	}, "m"))
	if err != nil {
		t.Fatalf("postResponsesRaw: %v", err)
	}
	defer raw.Close()
	id, err := newRespID()
	if err != nil {
		t.Fatalf("newRespID: %v", err)
	}
	if !respIDPat.MatchString(id) {
		t.Fatalf("resp id format = %q", id)
	}
	payload, err := aggregateResponses(raw, id, "m", 12345)
	if err != nil {
		t.Fatalf("aggregateResponses: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got["id"] != id || got["model"] != "m" {
		t.Fatalf("envelope = id=%v model=%v, want %q/m", got["id"], got["model"], id)
	}
	if got["created_at"].(float64) != 12345 {
		t.Fatalf("created_at = %v, want 12345", got["created_at"])
	}
	output, _ := got["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len = %d, want 2", len(output))
	}
	reasoning := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["encrypted_content"] != enc {
		t.Fatalf("reasoning item = %+v, want byte-identical encrypted_content", reasoning)
	}
	msg := output[1].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("msg type = %v", msg["type"])
	}
}

func TestResponsesUpstreamErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		wantT  string
		wantM  string
		wantH  bool
	}{
		{"403 FreeTierError", 403, `{"type":"error","error":{"type":"FreeTierError","message":"nope"}}`, "FreeTierError", "nope", true},
		{"401 AuthError", 401, `{"type":"error","error":{"type":"AuthError","message":"bad key"}}`, "AuthError", "bad key", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
			_, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{
				Model: "m", Input: "hi",
			}, "m"))
			var uerr *provider.UpstreamError
			if !errors.As(err, &uerr) {
				t.Fatalf("err type = %T, want *UpstreamError", err)
			}
			if uerr.Status != tt.status || uerr.Type != tt.wantT || uerr.Message != tt.wantM {
				t.Fatalf("uerr = %+v", uerr)
			}
			if tt.wantH && uerr.Hint == "" {
				t.Fatal("403 must carry UA-pin hint")
			}
			if hits.Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1 (no 4xx retry)", hits.Load())
			}
		})
	}
}

func TestResponsesNoCompletedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	}))
	defer srv.Close()
	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	raw, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{Model: "m", Input: "hi"}, "m"))
	if err != nil {
		t.Fatalf("postResponsesRaw: %v", err)
	}
	defer raw.Close()
	_, err = aggregateResponses(raw, "resp_x", "m", 1)
	if err == nil || !strings.Contains(err.Error(), "terminal event not found") {
		t.Fatalf("err = %v, want terminal event not found", err)
	}
}

func TestResponsesIncompleteEventFolds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
				"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\"}\n\n" +
				"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_up\",\"model\":\"m\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"cut\"}]}]}}\n\n",
		))
	}))
	defer srv.Close()
	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	raw, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{Model: "m", Input: "hi"}, "m"))
	if err != nil {
		t.Fatalf("postResponsesRaw: %v", err)
	}
	defer raw.Close()
	payload, err := aggregateResponses(raw, "resp_z", "m", 7)
	if err != nil {
		t.Fatalf("aggregateResponses: %v (incomplete must fold like completed)", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "incomplete" || got["id"] != "resp_z" || got["model"] != "m" {
		t.Fatalf("payload = %v, want status incomplete with rewritten envelope", got)
	}
	if got["created_at"].(float64) != 7 {
		t.Fatalf("created_at = %v, want 7", got["created_at"])
	}
	output, _ := got["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len = %d, want 1 (items pass through byte-identical)", len(output))
	}
	msg := output[0].(map[string]any)
	text := msg["content"].([]any)[0].(map[string]any)["text"]
	if msg["type"] != "message" || text != "cut" {
		t.Fatalf("output item = %v, want message with text cut", output[0])
	}
	details, _ := got["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v, want reason max_output_tokens pass-through", got["incomplete_details"])
	}
}

func TestResponsesFailedEventSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"quota exhausted\"}}}\n\n",
		))
	}))
	defer srv.Close()
	c := &zenChatClient{client: srv.Client(), recipe: recipeFromOptions(nil, srv.URL)}
	raw, err := c.postResponsesRaw(context.Background(), buildRespBody(provider.ResponsesRequest{Model: "m", Input: "hi"}, "m"))
	if err != nil {
		t.Fatalf("postResponsesRaw: %v", err)
	}
	defer raw.Close()
	_, err = aggregateResponses(raw, "resp_f", "m", 1)
	var ue *provider.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want UpstreamError", err)
	}
	if ue.Status != 502 || ue.Message != "quota exhausted" {
		t.Fatalf("upstream error = %+v, want 502 quota exhausted", ue)
	}
}
