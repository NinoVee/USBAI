// Command fakellama imitates llama-server's HTTP API for tests.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
)

func main() {
	port := flag.Int("port", 0, "")
	embedding := flag.Bool("embedding", false, "")
	flag.String("model", "", "")
	flag.String("host", "", "")
	flag.Int("ctx-size", 0, "")
	flag.Int("n-gpu-layers", 0, "")
	flag.Int("batch-size", 0, "")
	flag.Int("ubatch-size", 0, "")
	flag.Int("threads", 0, "")
	flag.Bool("jinja", false, "")
	flag.Parse()

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })
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
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		last := req.Messages[len(req.Messages)-1].Content
		reply := fmt.Sprintf("Got %d messages. ", len(req.Messages))
		if strings.Contains(last, "Document excerpts:") {
			reply += "Context: " + strings.ReplaceAll(last[:min(len(last), 300)], "\n", " ")
		} else {
			reply += "No documents."
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]string) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		send(map[string]string{"reasoning_content": "hmm"})
		for _, word := range strings.SplitAfter(reply, " ") {
			send(map[string]string{"content": word})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil)
}
