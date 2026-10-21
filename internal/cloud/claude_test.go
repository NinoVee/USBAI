package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ninovee/usbai/internal/llama"
)

// sse writes one server-sent event in the Messages API stream format.
func sse(w io.Writer, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// fakeClaude answers the first request with a thinking block and a tool
// call, and later ones with text. It records each request body.
func fakeClaude(t *testing.T, bodies *[]map[string]any, keys *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		*bodies = append(*bodies, body)
		*keys = append(*keys, r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": body["model"], "content": []any{},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
		if len(*bodies) == 1 {
			sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "signature_delta", "signature": "sig-123"}})
			sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 1,
				"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "calculator", "input": map[string]any{}}})
			sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 1,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"expression":"2+2"}`}})
			sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
			sse(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 5}})
		} else {
			sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""}})
			for _, part := range []string{"The answer ", "is 4."} {
				sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
					"delta": map[string]any{"type": "text_delta", "text": part}})
			}
			sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			sse(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 5}})
		}
		sse(w, "message_stop", map[string]any{"type": "message_stop"})
	}))
}

func TestClaudeToolLoop(t *testing.T) {
	var bodies []map[string]any
	var keys []string
	srv := fakeClaude(t, &bodies, &keys)
	defer srv.Close()
	old := ClaudeBaseURL
	ClaudeBaseURL = srv.URL
	defer func() { ClaudeBaseURL = old }()

	tools := []llama.Tool{{Type: "function", Function: llama.ToolFunction{Name: "calculator", Description: "Calculate",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"expression": map[string]any{"type": "string"}}, "required": []string{"expression"}}}}}
	msgs := []llama.Message{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: []llama.Part{
			{Type: "text", Text: "What is 2+2? And this image?"},
			{Type: "image_url", ImageURL: &llama.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
		}},
	}
	var text strings.Builder
	onDelta := func(d llama.Delta) error { text.WriteString(d.Content); return nil }

	calls, raw, err := ClaudeStream(context.Background(), "sk-test", "claude-opus-5-5", msgs, llama.ChatOptions{Tools: tools, ToolChoice: "required"}, onDelta)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].ID != "toolu_1" || calls[0].Function.Name != "calculator" || calls[0].Function.Arguments != `{"expression":"2+2"}` {
		t.Fatalf("tool calls: %+v", calls)
	}
	first := bodies[0]
	if keys[0] != "sk-test" || first["model"] != "claude-opus-5-5" || first["stream"] != true {
		t.Fatalf("request: key %q body %v", keys[0], first)
	}
	if _, ok := first["temperature"]; ok {
		t.Error("temperature must not be sent to current Claude models")
	}
	if _, ok := first["tool_choice"]; ok {
		t.Error("forced tool choice must not be sent to current Claude models")
	}
	if first["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want default", first["fallbacks"])
	}
	sys, _ := json.Marshal(first["system"])
	if !strings.Contains(string(sys), "Be brief.") || !strings.Contains(string(sys), "call one of the provided tools") {
		t.Errorf("system = %s", sys)
	}
	content, _ := json.Marshal(first["messages"])
	if !strings.Contains(string(content), `"media_type":"image/png"`) {
		t.Errorf("image not sent: %s", content)
	}

	// Second step: the assistant turn goes back as Claude sent it (with
	// its thinking block), and the tool result follows in a user turn.
	msgs = append(msgs,
		llama.Message{Role: "assistant", ToolCalls: calls, Raw: raw},
		llama.Message{Role: "tool", ToolCallID: "toolu_1", Content: "4"},
	)
	calls, _, err = ClaudeStream(context.Background(), "sk-test", "claude-opus-5-5", msgs, llama.ChatOptions{Tools: tools}, onDelta)
	if err != nil || len(calls) != 0 {
		t.Fatalf("second step: calls %v err %v", calls, err)
	}
	if text.String() != "The answer is 4." {
		t.Fatalf("text = %q", text.String())
	}
	var second struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	b, _ := json.Marshal(bodies[1])
	json.Unmarshal(b, &second)
	if n := len(second.Messages); n != 3 {
		t.Fatalf("want user, assistant, user turns; got %d: %s", n, b)
	}
	asst := second.Messages[1]
	if asst.Role != "assistant" || asst.Content[0]["type"] != "thinking" || asst.Content[0]["signature"] != "sig-123" || asst.Content[1]["type"] != "tool_use" {
		t.Fatalf("assistant turn not replayed as sent: %s", b)
	}
	res := second.Messages[2]
	if res.Role != "user" || res.Content[0]["type"] != "tool_result" || res.Content[0]["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool result turn: %s", b)
	}
}

func TestClaudeBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()
	old := ClaudeBaseURL
	ClaudeBaseURL = srv.URL
	defer func() { ClaudeBaseURL = old }()
	_, _, err := ClaudeStream(context.Background(), "bad", "claude-haiku-5-5", []llama.Message{{Role: "user", Content: "hi"}}, llama.ChatOptions{}, func(llama.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "key was rejected") {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitDataURL(t *testing.T) {
	if m, d, ok := splitDataURL("data:image/jpeg;base64,AAAA"); !ok || m != "image/jpeg" || d != "AAAA" {
		t.Fatalf("got %q %q %v", m, d, ok)
	}
	if _, _, ok := splitDataURL("data:image/bmp;base64,AAAA"); ok {
		t.Fatal("bmp accepted")
	}
}
