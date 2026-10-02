package app

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ninovee/usbai/internal/llama"
	"github.com/ninovee/usbai/internal/rag"
)

// ChatRequest is a user turn from the UI.
type ChatRequest struct {
	ChatID  string   `json:"chat_id"`
	Message string   `json:"message"`
	UseDocs bool     `json:"use_docs"`
	DocIDs  []string `json:"doc_ids"` // focus documents; empty means search all
}

// ChatEvents receives streamed progress for one turn.
type ChatEvents interface {
	Meta(chatID string, sources []Source) error
	Thinking() error
	Token(text string) error
}

const (
	replyReserve   = 1024 // tokens kept free for the answer
	retrievalTopK  = 6
	excerptPreview = 240
)

func approxTokens(s string) int { return utf8.RuneCountInString(s)/4 + 1 }

// Chat runs one turn: retrieve context, stream the model's answer, and save
// the conversation to the vault.
func (a *App) Chat(req ChatRequest, ev ChatEvents) error {
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		return errors.New("empty message")
	}
	if _, err := a.unlocked(); err != nil {
		return err
	}
	url, model, ready := a.chatEngine()
	if !ready {
		return errors.New("the AI model is still loading — try again in a moment")
	}

	var chat Chat
	if req.ChatID != "" {
		if err := validID(req.ChatID); err != nil {
			return err
		}
		c, err := a.GetChat(req.ChatID)
		if err != nil {
			return fmt.Errorf("load chat: %w", err)
		}
		chat = c
	} else {
		title := msg
		if r := []rune(title); len(r) > 60 {
			title = string(r[:60]) + "…"
		}
		chat = Chat{ID: newID(), Title: title, Created: time.Now()}
	}

	ctxTokens := model.Context
	if ctxTokens <= 0 {
		ctxTokens = 4096
	}
	budget := ctxTokens - replyReserve

	system := a.systemPrompt()
	budget -= approxTokens(system) + approxTokens(msg)

	docContext, sources := "", []Source(nil)
	if req.UseDocs || len(req.DocIDs) > 0 {
		docContext, sources = a.retrieve(msg, req.DocIDs, budget/2)
		budget -= approxTokens(docContext)
	}

	// Most recent history that fits the remaining budget.
	var history []llama.Message
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		m := chat.Messages[i]
		cost := approxTokens(m.Content)
		if cost > budget {
			break
		}
		budget -= cost
		history = append([]llama.Message{{Role: m.Role, Content: m.Content}}, history...)
	}

	msgs := []llama.Message{{Role: "system", Content: system}}
	msgs = append(msgs, history...)
	user := msg
	if docContext != "" {
		user = "Document excerpts:\n\n" + docContext + "\n\n---\n\nQuestion: " + msg
	}
	msgs = append(msgs, llama.Message{Role: "user", Content: user})

	if err := ev.Meta(chat.ID, sources); err != nil {
		return err
	}
	var answer strings.Builder
	thinking := false
	err := llama.ChatStream(a.ctx, url, msgs, 0.6, func(d llama.Delta) error {
		if d.Reasoning != "" && !thinking {
			thinking = true
			if err := ev.Thinking(); err != nil {
				return err
			}
		}
		if d.Content == "" {
			return nil
		}
		answer.WriteString(d.Content)
		return ev.Token(d.Content)
	})
	reply := strings.TrimSpace(stripThink(answer.String()))
	if reply == "" && err != nil {
		return err
	}

	now := time.Now()
	chat.Messages = append(chat.Messages,
		ChatMessage{Role: "user", Content: msg, Time: now},
		ChatMessage{Role: "assistant", Content: reply, Sources: sources, Time: now},
	)
	chat.Updated = now
	if serr := a.saveChat(chat); serr != nil {
		return serr
	}
	return err
}

// stripThink removes <think>…</think> blocks from models that emit them inline.
func stripThink(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
}

func (a *App) systemPrompt() string {
	var b strings.Builder
	b.WriteString(a.cfg.SystemPrompt)
	b.WriteString("\n\nToday's date is ")
	b.WriteString(time.Now().Format("Monday, 2 January 2006"))
	b.WriteString(".")
	mem, _ := a.Memory()
	if len(mem) > 0 {
		b.WriteString("\n\nThings the user has asked you to remember:\n")
		for _, m := range mem {
			b.WriteString("- ")
			b.WriteString(m.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// retrieve builds the document context for a question within maxTokens.
// If the user focused specific documents and they fit, their full text is
// used (best for "summarize this contract"); otherwise the most relevant
// excerpts are retrieved.
func (a *App) retrieve(query string, docIDs []string, maxTokens int) (string, []Source) {
	v, err := a.unlocked()
	if err != nil || maxTokens <= 0 {
		return "", nil
	}
	a.dataMu.RLock()
	index := a.index
	names := map[string]string{}
	for id, d := range a.docs {
		names[id] = d.Name
	}
	a.dataMu.RUnlock()

	filter := map[string]bool{}
	for _, id := range docIDs {
		if _, ok := names[id]; ok {
			filter[id] = true
		}
	}

	// Whole-document mode.
	if len(filter) > 0 {
		var b strings.Builder
		var sources []Source
		fits := true
		for id := range filter {
			text, err := v.Get("docs/" + id + "/text")
			if err != nil {
				fits = false
				break
			}
			fmt.Fprintf(&b, "[Document: %s]\n%s\n\n", names[id], text)
			sources = append(sources, Source{DocID: id, DocName: names[id], Seq: -1, Excerpt: preview(string(text))})
			if approxTokens(b.String()) > maxTokens {
				fits = false
				break
			}
		}
		if fits {
			return strings.TrimSpace(b.String()), sources
		}
	}

	if index == nil || index.Len() == 0 {
		return "", nil
	}
	var qvec []float32
	if _, em, ready := a.embedEngine(); ready {
		vecs, ok, err := a.embedTexts([]string{query}, em.QueryPrefix)
		if err == nil && ok {
			qvec = vecs[0]
		}
	}
	results := index.Search(query, qvec, retrievalTopK, filter)
	return formatExcerpts(results, maxTokens)
}

func formatExcerpts(results []rag.Result, maxTokens int) (string, []Source) {
	var b strings.Builder
	var sources []Source
	for _, r := range results {
		block := fmt.Sprintf("[Document: %s, part %d]\n%s\n\n", r.DocName, r.Seq+1, r.Text)
		if approxTokens(b.String()+block) > maxTokens {
			break
		}
		b.WriteString(block)
		sources = append(sources, Source{DocID: r.DocID, DocName: r.DocName, Seq: r.Seq, Excerpt: preview(r.Text)})
	}
	return strings.TrimSpace(b.String()), sources
}

func preview(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > excerptPreview {
		return string(r[:excerptPreview]) + "…"
	}
	return string(r)
}
