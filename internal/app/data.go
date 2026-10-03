package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ninovee/usbai/internal/llama"
	"github.com/ninovee/usbai/internal/rag"
	"github.com/ninovee/usbai/internal/vault"
)

// Vault object layout:
//
//	docs/<id>/meta    DocMeta
//	docs/<id>/file    original upload
//	docs/<id>/text    extracted text
//	docs/<id>/pieces  []rag.Piece (chunks + embeddings)
//	chats/<id>        Chat
//	memory            []MemoryItem

// DocMeta describes an uploaded document.
type DocMeta struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Size     int       `json:"size"`
	Chars    int       `json:"chars"`
	Pieces   int       `json:"pieces"`
	Embedded bool      `json:"embedded"`
	Added    time.Time `json:"added"`
}

// MemoryItem is a fact the user asked the assistant to remember.
type MemoryItem struct {
	ID    string    `json:"id"`
	Text  string    `json:"text"`
	Added time.Time `json:"added"`
}

// ChatMessage is one turn of a stored conversation.
type ChatMessage struct {
	Role    string     `json:"role"`
	Content string     `json:"content"`
	Sources []Source   `json:"sources,omitempty"`
	Images  []string   `json:"images,omitempty"` // vault image ids
	Steps   []ToolStep `json:"steps,omitempty"`  // agent tool use
	Time    time.Time  `json:"time"`
}

// Source is a document excerpt used to answer.
type Source struct {
	DocID   string `json:"doc_id"`
	DocName string `json:"doc_name"`
	Seq     int    `json:"seq"`
	Excerpt string `json:"excerpt"`
}

// Chat is a stored conversation.
type Chat struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	AgentID  string        `json:"agent_id,omitempty"`
	Created  time.Time     `json:"created"`
	Updated  time.Time     `json:"updated"`
	Messages []ChatMessage `json:"messages"`
}

var errLocked = errors.New("vault is locked")

func newID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) vaultDir() string { return filepath.Join(a.dataDir, "vault") }

// Initialized reports whether the user has created a vault on this drive.
func (a *App) Initialized() bool { return vault.Exists(a.vaultDir()) }

// unlock opens (or on first run creates) the vault and loads the user's
// documents and memory into RAM.
func (a *App) unlock(passphrase string, create bool) error {
	var (
		v   *vault.Vault
		err error
	)
	if create {
		v, err = vault.Create(a.vaultDir(), passphrase)
	} else {
		v, err = vault.Open(a.vaultDir(), passphrase)
	}
	if err != nil {
		return err
	}

	index := rag.NewIndex()
	docs := map[string]DocMeta{}
	for _, name := range v.List("docs/") {
		if !strings.HasSuffix(name, "/meta") {
			continue
		}
		var m DocMeta
		if err := v.GetJSON(name, &m); err != nil {
			a.logf("skipping unreadable document %s: %v", name, err)
			continue
		}
		var pieces []rag.Piece
		if err := v.GetJSON("docs/"+m.ID+"/pieces", &pieces); err != nil {
			a.logf("skipping document %s with unreadable index: %v", m.Name, err)
			continue
		}
		index.Add(pieces)
		docs[m.ID] = m
	}
	var memory []MemoryItem
	if err := v.GetJSON("memory", &memory); err != nil && !errors.Is(err, vault.ErrNotFound) {
		return err
	}

	a.dataMu.Lock()
	a.vault, a.index, a.docs, a.memory = v, index, docs, memory
	a.dataMu.Unlock()
	go a.backfillEmbeddings()
	return nil
}

// lock drops the key and all decrypted data from memory.
func (a *App) lock() {
	a.dataMu.Lock()
	a.vault, a.index, a.docs, a.memory = nil, nil, nil, nil
	a.dataMu.Unlock()
}

func (a *App) unlocked() (*vault.Vault, error) {
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	if a.vault == nil {
		return nil, errLocked
	}
	return a.vault, nil
}

// ---- Documents ----

const (
	chunkSize    = 1200
	chunkOverlap = 200
	embedBatch   = 16
)

// embedTexts embeds texts in batches. ok is false if no embedding model is
// running, in which case documents are still keyword-searchable.
func (a *App) embedTexts(texts []string, prefix string) ([][]float32, bool, error) {
	url, _, ready := a.embedEngine()
	if !ready {
		return nil, false, nil
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += embedBatch {
		end := min(i+embedBatch, len(texts))
		batch := make([]string, 0, end-i)
		for _, t := range texts[i:end] {
			batch = append(batch, prefix+t)
		}
		vecs, err := llama.Embed(a.ctx, url, batch)
		if err != nil {
			return nil, false, err
		}
		out = append(out, vecs...)
	}
	return out, true, nil
}

// AddDocument extracts, chunks, embeds and stores a document.
func (a *App) AddDocument(name string, data []byte) (DocMeta, error) {
	v, err := a.unlocked()
	if err != nil {
		return DocMeta{}, err
	}
	text, err := rag.Extract(name, data)
	if err != nil {
		return DocMeta{}, err
	}
	chunks := rag.Chunk(text, chunkSize, chunkOverlap)
	meta := DocMeta{ID: newID(), Name: name, Size: len(data), Chars: len([]rune(text)), Pieces: len(chunks), Added: time.Now()}

	_, em, _ := a.embedEngine()
	vecs, embedded, err := a.embedTexts(chunks, em.DocumentPrefix)
	if err != nil {
		a.logf("embedding %s failed, using keyword search only: %v", name, err)
	}
	pieces := make([]rag.Piece, len(chunks))
	for i, c := range chunks {
		pieces[i] = rag.Piece{DocID: meta.ID, DocName: name, Seq: i, Text: c}
		if embedded {
			pieces[i].Vec = vecs[i]
		}
	}
	meta.Embedded = embedded

	prefix := "docs/" + meta.ID + "/"
	if err := v.Put(prefix+"file", data); err != nil {
		return DocMeta{}, err
	}
	if err := v.Put(prefix+"text", []byte(text)); err != nil {
		return DocMeta{}, err
	}
	if err := v.PutJSON(prefix+"pieces", pieces); err != nil {
		return DocMeta{}, err
	}
	if err := v.PutJSON(prefix+"meta", meta); err != nil {
		return DocMeta{}, err
	}

	a.dataMu.Lock()
	if a.vault == v {
		a.index.Add(pieces)
		a.docs[meta.ID] = meta
	}
	a.dataMu.Unlock()
	return meta, nil
}

// DeleteDocument removes a document and its index entries.
func (a *App) DeleteDocument(id string) error {
	v, err := a.unlocked()
	if err != nil {
		return err
	}
	for _, part := range []string{"meta", "pieces", "text", "file"} {
		if err := v.Delete("docs/" + id + "/" + part); err != nil {
			return err
		}
	}
	a.dataMu.Lock()
	if a.vault == v {
		a.index.RemoveDoc(id)
		delete(a.docs, id)
	}
	a.dataMu.Unlock()
	return nil
}

// Documents lists documents, newest first.
func (a *App) Documents() ([]DocMeta, error) {
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	if a.vault == nil {
		return nil, errLocked
	}
	out := make([]DocMeta, 0, len(a.docs))
	for _, d := range a.docs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.After(out[j].Added) })
	return out, nil
}

// DocumentFile returns a document's original bytes.
func (a *App) DocumentFile(id string) (DocMeta, []byte, error) {
	v, err := a.unlocked()
	if err != nil {
		return DocMeta{}, nil, err
	}
	a.dataMu.RLock()
	meta, ok := a.docs[id]
	a.dataMu.RUnlock()
	if !ok {
		return DocMeta{}, nil, vault.ErrNotFound
	}
	b, err := v.Get("docs/" + id + "/file")
	return meta, b, err
}

// backfillEmbeddings embeds documents added while no embedding model was
// running (e.g. uploaded while it was still loading).
func (a *App) backfillEmbeddings() {
	url, em, ready := a.embedEngine()
	if !ready || url == "" {
		return
	}
	docs, err := a.Documents()
	if err != nil {
		return
	}
	v, err := a.unlocked()
	if err != nil {
		return
	}
	for _, d := range docs {
		if d.Embedded || d.Pieces == 0 {
			continue
		}
		var pieces []rag.Piece
		if err := v.GetJSON("docs/"+d.ID+"/pieces", &pieces); err != nil {
			continue
		}
		texts := make([]string, len(pieces))
		for i, p := range pieces {
			texts[i] = p.Text
		}
		vecs, ok, err := a.embedTexts(texts, em.DocumentPrefix)
		if err != nil || !ok {
			a.logf("backfill embeddings for %s: %v", d.Name, err)
			return
		}
		for i := range pieces {
			pieces[i].Vec = vecs[i]
		}
		d.Embedded = true
		if err := v.PutJSON("docs/"+d.ID+"/pieces", pieces); err != nil {
			return
		}
		if err := v.PutJSON("docs/"+d.ID+"/meta", d); err != nil {
			return
		}
		a.dataMu.Lock()
		if a.vault == v {
			if _, still := a.docs[d.ID]; still {
				a.index.RemoveDoc(d.ID)
				a.index.Add(pieces)
				a.docs[d.ID] = d
			}
		}
		a.dataMu.Unlock()
		a.logf("Embedded %s", d.Name)
	}
}

// ---- Memory ----

// Memory returns the remembered facts.
func (a *App) Memory() ([]MemoryItem, error) {
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	if a.vault == nil {
		return nil, errLocked
	}
	return append([]MemoryItem{}, a.memory...), nil
}

// Remember adds a fact to memory.
func (a *App) Remember(text string) (MemoryItem, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return MemoryItem{}, errors.New("empty memory")
	}
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	if a.vault == nil {
		return MemoryItem{}, errLocked
	}
	item := MemoryItem{ID: newID(), Text: text, Added: time.Now()}
	mem := append(append([]MemoryItem{}, a.memory...), item)
	if err := a.vault.PutJSON("memory", mem); err != nil {
		return MemoryItem{}, err
	}
	a.memory = mem
	return item, nil
}

// Forget removes a memory item.
func (a *App) Forget(id string) error {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	if a.vault == nil {
		return errLocked
	}
	mem := make([]MemoryItem, 0, len(a.memory))
	for _, m := range a.memory {
		if m.ID != id {
			mem = append(mem, m)
		}
	}
	if err := a.vault.PutJSON("memory", mem); err != nil {
		return err
	}
	a.memory = mem
	return nil
}

// ---- Chats ----

// ChatSummary is a chat without its messages.
type ChatSummary struct {
	ID      string    `json:"id"`
	AgentID string    `json:"agent_id,omitempty"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
}

// Chats lists conversations, most recent first.
func (a *App) Chats() ([]ChatSummary, error) {
	v, err := a.unlocked()
	if err != nil {
		return nil, err
	}
	var out []ChatSummary
	for _, name := range v.List("chats/") {
		var c Chat
		if err := v.GetJSON(name, &c); err != nil {
			continue
		}
		out = append(out, ChatSummary{ID: c.ID, AgentID: c.AgentID, Title: c.Title, Updated: c.Updated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// GetChat loads one conversation.
func (a *App) GetChat(id string) (Chat, error) {
	v, err := a.unlocked()
	if err != nil {
		return Chat{}, err
	}
	var c Chat
	err = v.GetJSON("chats/"+id, &c)
	return c, err
}

func (a *App) saveChat(c Chat) error {
	v, err := a.unlocked()
	if err != nil {
		return err
	}
	return v.PutJSON("chats/"+c.ID, c)
}

// DeleteChat removes a conversation.
func (a *App) DeleteChat(id string) error {
	v, err := a.unlocked()
	if err != nil {
		return err
	}
	var c Chat
	if err := v.GetJSON("chats/"+id, &c); err == nil {
		for _, m := range c.Messages {
			for _, img := range m.Images {
				v.Delete("images/" + img)
			}
		}
	}
	return v.Delete("chats/" + id)
}

func validID(id string) error {
	if id == "" || len(id) > 64 || strings.ContainsAny(id, "/\\.") {
		return fmt.Errorf("invalid id %q", id)
	}
	return nil
}
