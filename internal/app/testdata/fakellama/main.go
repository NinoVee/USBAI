// Command fakellama imitates llama-server's HTTP API for tests.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	port := flag.Int("port", 0, "")
	embedding := flag.Bool("embedding", false, "")
	model := flag.String("model", "", "")
	flag.String("host", "", "")
	ctxSize := flag.Int("ctx-size", 0, "")
	flag.Int("n-gpu-layers", 0, "")
	flag.Int("batch-size", 0, "")
	flag.Int("ubatch-size", 0, "")
	flag.Int("threads", 0, "")
	flag.Bool("jinja", false, "")
	flag.String("mmproj", "", "")
	flag.Parse()
	if strings.Contains(*model, "broken") {
		fmt.Fprintln(os.Stderr, "llama_model_load: error loading model: tensor 'blk.17.ffn_up.weight' data is not within the file bounds, model is corrupted or incomplete")
		os.Exit(1)
	}

	// FAKELLAMA_DELAY (e.g. "5s") simulates a slow model load.
	delay, _ := time.ParseDuration(os.Getenv("FAKELLAMA_DELAY"))
	start := time.Now()
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < delay {
			http.Error(w, `{"status":"loading model"}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	})
	http.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if !*embedding {
			http.Error(w, "embeddings disabled", 501)
			return
		}
		var req struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, s := range req.Input {
			v := make([]float32, 64)
			for _, w := range strings.Fields(strings.ToLower(s)) {
				h := fnv.New32a()
				h.Write([]byte(strings.Trim(w, ".,:?!")))
				v[h.Sum32()%64]++
			}
			data = append(data, item{i, v})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
			Tools []struct {
				Function struct{ Name string }
			}
			ToolChoice string `json:"tool_choice"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		// Refuse requests longer than the context window like llama-server
		// does, counting text more densely than the app's own estimate.
		tokens := 0
		for _, m := range req.Messages {
			tokens += 4
			if len(m.Content) > 0 && m.Content[0] == '"' { // text; not images or audio
				tokens += len(m.Content) / 3
			}
		}
		if *ctxSize > 0 && tokens > *ctxSize {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"code":400,"message":"request (%d tokens) exceeds the available context size (%d tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":%d,"n_ctx":%d}}`, tokens, *ctxSize, tokens, *ctxSize)
			return
		}
		lastMsg := req.Messages[len(req.Messages)-1]
		// Content is a string, or parts when images are attached.
		var last string
		images := 0
		if json.Unmarshal(lastMsg.Content, &last) != nil {
			var parts []struct {
				Type string
				Text string
			}
			json.Unmarshal(lastMsg.Content, &parts)
			for _, p := range parts {
				if p.Type == "image_url" {
					images++
				} else if p.Type == "input_audio" {
					last = "[audio]"
				} else {
					last += p.Text
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]any) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}

		// tool_choice "required" calls the first offered tool with the
		// user's words as the query; it records which tools were offered.
		if req.ToolChoice == "required" && lastMsg.Role == "user" && len(req.Tools) > 0 {
			var offered []string
			for _, t := range req.Tools {
				offered = append(offered, t.Function.Name)
			}
			args, _ := json.Marshal(map[string]string{"query": last, "offered": strings.Join(offered, ",")})
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "r1", "function": map[string]any{"name": req.Tools[0].Function.Name, "arguments": string(args)}}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}

		// "calc: EXPR" asks for the calculator tool when it is offered;
		// "loop: EXPR" keeps asking for it, like a confused small model.
		for _, m := range req.Messages {
			var text string
			if json.Unmarshal(m.Content, &text) == nil && m.Role == "user" && len(req.Tools) > 0 {
				if i := strings.Index(text, "loop:"); i >= 0 {
					last, lastMsg.Role = "calc:"+text[i+5:], "user"
				}
			}
		}
		// "use: TOOL {json args}" calls any offered tool, for tests.
		if i := strings.Index(last, "use: "); i >= 0 && lastMsg.Role == "user" && len(req.Tools) > 0 {
			name, args, _ := strings.Cut(strings.TrimSpace(last[i+5:]), " ")
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "u1", "function": map[string]any{"name": name, "arguments": args}}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		if i := strings.Index(last, "calc:"); i >= 0 && lastMsg.Role == "user" && len(req.Tools) > 0 {
			args, _ := json.Marshal(map[string]string{"expression": strings.TrimSpace(last[i+5:])})
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "function": map[string]any{"name": "calculator", "arguments": ""}}}})
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": string(args)}}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}

		reply := fmt.Sprintf("Got %d messages. ", len(req.Messages))
		system := string(req.Messages[0].Content)
		switch {
		case strings.Contains(last, "greet-too"):
			// Imitate a small model that writes its own greeting even
			// though the system prompt told it not to.
			reply = "Wat up homie? Wat it do?\nI'm just kickin' it, no cap.\nThat's real spit, no cap.\nThat's real spit, no cap."
		case last == "[audio]":
			reply = "language English<asr_text>Hello from the microphone."
		case strings.Contains(system, "You are an OCR engine") && images > 0:
			reply = "```\nMEMO 7: the budget is $342,500.\n```"
		case strings.Contains(system, "You describe images") && images > 0:
			reply = "A screenshot of an error dialog that says ERROR 42."
		case strings.Contains(last, "described by the image reader"):
			reply += "Main model read: " + last[strings.Index(last, "A screenshot"):strings.Index(last, "ERROR 42.")+9]
		case lastMsg.Role == "tool":
			reply += "Tool said: " + last
		case images > 0:
			reply += fmt.Sprintf("I see %d image(s).", images)
		case strings.Contains(last, "Document excerpts:"):
			reply += "Context: " + strings.ReplaceAll(last[:min(len(last), 300)], "\n", " ")
		default:
			reply += "No documents."
		}
		if strings.Contains(string(req.Messages[0].Content), "acting as the agent") {
			reply += " [agent]"
		}
		send(map[string]any{"reasoning_content": "hmm"})
		for _, word := range strings.SplitAfter(reply, " ") {
			send(map[string]any{"content": word})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil)
}
