package llama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is an OpenAI-style chat message. Content is a string, or a
// []Part when the message carries images.
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Part is one piece of a multimodal message.
type Part struct {
	Type     string    `json:"type"` // "text" or "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries an image as a data: URL.
type ImageURL struct {
	URL string `json:"url"`
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction is a tool's name, purpose and JSON-schema parameters.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the function and its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Delta is one streamed piece of a chat completion. Reasoning models
// (e.g. Qwen3 in thinking mode) put their thinking in Reasoning.
type Delta struct {
	Content   string
	Reasoning string
}

// ChatOptions tunes one completion.
type ChatOptions struct {
	Temperature float64
	Tools       []Tool
	// ToolChoice is "", "auto", "none" or "required" ("required" makes the
	// model call a tool this round).
	ToolChoice string
	// MaxTokens caps the reply length (0 = no cap).
	MaxTokens int
}

// ChatStream sends a chat completion request and calls onDelta for each
// streamed token batch. It returns any tool calls the model made once the
// stream ends.
func ChatStream(ctx context.Context, baseURL string, msgs []Message, opts ChatOptions, onDelta func(Delta) error) ([]ToolCall, error) {
	payload := map[string]any{
		"messages":    msgs,
		"stream":      true,
		"temperature": opts.Temperature,
	}
	if opts.MaxTokens > 0 {
		payload["max_tokens"] = opts.MaxTokens
	}
	if len(opts.Tools) > 0 {
		payload["tools"] = opts.Tools
		if opts.ToolChoice != "" {
			payload["tool_choice"] = opts.ToolChoice
		}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("model server: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	// Tool calls stream in fragments keyed by index.
	var calls []ToolCall
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("model server: %s", chunk.Error.Message)
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				for len(calls) <= tc.Index {
					calls = append(calls, ToolCall{Type: "function"})
				}
				call := &calls[tc.Index]
				if tc.ID != "" {
					call.ID = tc.ID
				}
				call.Function.Name += tc.Function.Name
				call.Function.Arguments += tc.Function.Arguments
			}
			d := Delta{Content: c.Delta.Content, Reasoning: c.Delta.ReasoningContent}
			if d.Content == "" && d.Reasoning == "" {
				continue
			}
			if err := onDelta(d); err != nil {
				return nil, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	return calls, nil
}

// Embed returns one embedding vector per input.
func Embed(ctx context.Context, baseURL string, inputs []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"input": inputs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embedding server: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding server returned %d vectors for %d inputs", len(out.Data), len(inputs))
	}
	vecs := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embedding server returned bad index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
