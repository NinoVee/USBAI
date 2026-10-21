// Package cloud talks to cloud chat models — Claude (Anthropic) and ChatGPT
// (OpenAI) — with the user's own API keys. It is only used when the user
// picks a cloud model; the drive's local models never go through here.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"github.com/ninovee/usbai/internal/llama"
)

// Endpoints are variables so tests can point them at local servers.
var (
	ClaudeBaseURL = "https://api.anthropic.com"
	OpenAIBaseURL = "https://api.openai.com"
)

// claudeMaxTokens caps one Claude reply when the caller sets no cap.
const claudeMaxTokens = 64000

// fallbackModels are the Claude models that accept the server-side
// fallback, which re-serves a request a safety check declines on another
// model instead of ending the reply.
var fallbackModels = map[string]bool{
	"claude-fable-5-1": true, "claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true,
}

// ClaudeStream runs one Claude turn, streaming the reply text to onDelta.
// It returns the tool calls Claude made and its assistant turn in Claude's
// own form, to send back unchanged on the next call (it carries thinking
// blocks that Claude needs to continue after a tool call).
func ClaudeStream(ctx context.Context, key, model string, msgs []llama.Message, opts llama.ChatOptions, onDelta func(llama.Delta) error) ([]llama.ToolCall, any, error) {
	params, err := claudeParams(model, msgs, opts)
	if err != nil {
		return nil, nil, err
	}
	client := anthropic.NewClient(option.WithAPIKey(key), option.WithBaseURL(ClaudeBaseURL))
	stream := client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var msg anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, nil, err
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.BetaTextDelta); ok && t.Text != "" {
				if err := onDelta(llama.Delta{Content: t.Text}); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, nil, claudeError(err)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return nil, nil, errors.New("Claude declined to answer this request")
	}
	var calls []llama.ToolCall
	for _, b := range msg.Content {
		if tu, ok := b.AsAny().(anthropic.BetaToolUseBlock); ok {
			args := tu.JSON.Input.Raw()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, llama.ToolCall{ID: tu.ID, Type: "function", Function: llama.FunctionCall{Name: tu.Name, Arguments: args}})
		}
	}
	return calls, msg.ToParam(), nil
}

// claudeParams turns an OpenAI-style conversation into a Claude request.
func claudeParams(model string, msgs []llama.Message, opts llama.ChatOptions) (anthropic.BetaMessageNewParams, error) {
	p := anthropic.BetaMessageNewParams{Model: anthropic.Model(model), MaxTokens: claudeMaxTokens}
	if opts.MaxTokens > 0 {
		p.MaxTokens = int64(opts.MaxTokens)
	}
	if fallbackModels[model] {
		p.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		p.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	var system []string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = append(system, textOf(m.Content))
		case "user":
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return p, err
			}
			p.Messages = appendTurn(p.Messages, anthropic.BetaMessageParamRoleUser, blocks...)
		case "assistant":
			if raw, ok := m.Raw.(anthropic.BetaMessageParam); ok {
				p.Messages = appendTurn(p.Messages, anthropic.BetaMessageParamRoleAssistant, raw.Content...)
				continue
			}
			var blocks []anthropic.BetaContentBlockParamUnion
			if t := strings.TrimSpace(textOf(m.Content)); t != "" {
				blocks = append(blocks, anthropic.NewBetaTextBlock(t))
			}
			for _, c := range m.ToolCalls {
				var input any = map[string]any{}
				if strings.TrimSpace(c.Function.Arguments) != "" {
					input = json.RawMessage(c.Function.Arguments)
				}
				blocks = append(blocks, anthropic.NewBetaToolUseBlock(c.ID, input, c.Function.Name))
			}
			if len(blocks) > 0 {
				p.Messages = appendTurn(p.Messages, anthropic.BetaMessageParamRoleAssistant, blocks...)
			}
		case "tool":
			p.Messages = appendTurn(p.Messages, anthropic.BetaMessageParamRoleUser,
				anthropic.NewBetaToolResultBlock(m.ToolCallID, textOf(m.Content), false))
		}
	}
	if len(opts.Tools) > 0 {
		for _, t := range opts.Tools {
			tp := anthropic.BetaToolParam{Name: t.Function.Name, Description: param.NewOpt(t.Function.Description)}
			var schema struct {
				Properties any      `json:"properties"`
				Required   []string `json:"required"`
			}
			b, _ := json.Marshal(t.Function.Parameters)
			_ = json.Unmarshal(b, &schema)
			tp.InputSchema = anthropic.BetaToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required}
			p.Tools = append(p.Tools, anthropic.BetaToolUnionParam{OfTool: &tp})
		}
		switch opts.ToolChoice {
		case "none":
			p.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
		case "required":
			// Current Claude models don't accept a forced tool call; ask
			// for it in words instead.
			system = append(system, "Before answering, call one of the provided tools now.")
		}
	}
	if len(system) > 0 {
		p.System = []anthropic.BetaTextBlockParam{{Text: strings.Join(system, "\n\n")}}
	}
	return p, nil
}

// appendTurn adds blocks to the conversation, merging them into the last
// turn when it has the same role (Claude needs user and assistant turns
// to alternate, and all tool results of a step in one user turn).
func appendTurn(ms []anthropic.BetaMessageParam, role anthropic.BetaMessageParamRole, blocks ...anthropic.BetaContentBlockParamUnion) []anthropic.BetaMessageParam {
	if len(blocks) == 0 {
		return ms
	}
	if n := len(ms); n > 0 && ms[n-1].Role == role {
		ms[n-1].Content = append(ms[n-1].Content, blocks...)
		return ms
	}
	return append(ms, anthropic.BetaMessageParam{Role: role, Content: blocks})
}

// userBlocks converts a user message: text, and images as data: URLs.
func userBlocks(content any) ([]anthropic.BetaContentBlockParamUnion, error) {
	parts, ok := content.([]llama.Part)
	if !ok {
		if t := textOf(content); t != "" {
			return []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(t)}, nil
		}
		return nil, nil
	}
	var out []anthropic.BetaContentBlockParamUnion
	for _, pt := range parts {
		switch {
		case pt.Type == "text" && pt.Text != "":
			out = append(out, anthropic.NewBetaTextBlock(pt.Text))
		case pt.Type == "image_url" && pt.ImageURL != nil:
			media, data, ok := splitDataURL(pt.ImageURL.URL)
			if !ok {
				return nil, errors.New("Claude only accepts JPEG, PNG, GIF or WebP images")
			}
			out = append(out, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
				Data: data, MediaType: anthropic.BetaBase64ImageSourceMediaType(media),
			}))
		}
	}
	return out, nil
}

// splitDataURL splits "data:image/png;base64,AAAA" into its media type and
// base64 data.
func splitDataURL(u string) (media, data string, ok bool) {
	head, data, found := strings.Cut(u, ",")
	media = strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")
	switch media {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return media, data, found && strings.HasSuffix(head, ";base64")
	}
	return "", "", false
}

// textOf returns a message's text, joining the text parts of a multipart
// message.
func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []llama.Part:
		var b strings.Builder
		for _, p := range c {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	case nil:
		return ""
	}
	return fmt.Sprint(content)
}

// claudeError turns an API failure into a message the user can act on.
func claudeError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errors.New("the Claude API key was rejected — check it in Settings → Cloud AI")
		case http.StatusTooManyRequests:
			return errors.New("Claude's rate limit was reached — wait a moment and try again")
		case http.StatusNotFound:
			return fmt.Errorf("Claude doesn't know this model — %w", err)
		}
		if apiErr.StatusCode >= 500 {
			return errors.New("Claude's servers are busy or down right now — try again in a moment")
		}
	}
	return fmt.Errorf("Claude: %w", err)
}
