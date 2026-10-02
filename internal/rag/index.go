package rag

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Piece is one searchable chunk of a document. Vec is empty when no
// embedding model is available; keyword search still works then.
type Piece struct {
	DocID   string    `json:"doc_id"`
	DocName string    `json:"doc_name"`
	Seq     int       `json:"seq"`
	Text    string    `json:"text"`
	Vec     []float32 `json:"vec,omitempty"`
}

// Result is a search hit.
type Result struct {
	Piece
	Score float64 `json:"score"`
}

type entry struct {
	Piece
	terms map[string]int
	n     int // term count
}

// Index is an in-memory hybrid search index: BM25 keyword ranking fused with
// cosine similarity over embeddings (reciprocal rank fusion). Personal
// document collections are small enough that brute force is fast.
type Index struct {
	mu      sync.RWMutex
	entries []*entry
	df      map[string]int
	total   int
}

// NewIndex returns an empty index.
func NewIndex() *Index { return &Index{df: map[string]int{}} }

// Add indexes pieces. Vectors are normalized in place.
func (ix *Index) Add(pieces []Piece) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for _, p := range pieces {
		normalize(p.Vec)
		e := &entry{Piece: p, terms: map[string]int{}}
		for _, t := range Tokenize(p.Text) {
			e.terms[t]++
			e.n++
		}
		for t := range e.terms {
			ix.df[t]++
		}
		ix.total += e.n
		ix.entries = append(ix.entries, e)
	}
}

// RemoveDoc drops every piece of a document.
func (ix *Index) RemoveDoc(docID string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	kept := ix.entries[:0]
	for _, e := range ix.entries {
		if e.DocID != docID {
			kept = append(kept, e)
			continue
		}
		for t := range e.terms {
			if ix.df[t]--; ix.df[t] <= 0 {
				delete(ix.df, t)
			}
		}
		ix.total -= e.n
	}
	for i := len(kept); i < len(ix.entries); i++ {
		ix.entries[i] = nil
	}
	ix.entries = kept
}

// Len returns the number of indexed pieces.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.entries)
}

// Search returns the top k pieces for the query. qvec may be nil for
// keyword-only search. If docs is non-empty, only those documents are searched.
func (ix *Index) Search(query string, qvec []float32, k int, docs map[string]bool) []Result {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.entries) == 0 || k <= 0 {
		return nil
	}
	var cands []*entry
	for _, e := range ix.entries {
		if len(docs) == 0 || docs[e.DocID] {
			cands = append(cands, e)
		}
	}

	const rrfK = 60.0
	fused := map[*entry]float64{}

	// Keyword ranking (BM25).
	qterms := Tokenize(query)
	if len(qterms) > 0 {
		avg := float64(ix.total) / float64(len(ix.entries))
		N := float64(len(ix.entries))
		type scored struct {
			e *entry
			s float64
		}
		var kw []scored
		for _, e := range cands {
			s := 0.0
			for _, t := range qterms {
				tf := float64(e.terms[t])
				if tf == 0 {
					continue
				}
				df := float64(ix.df[t])
				idf := math.Log(1 + (N-df+0.5)/(df+0.5))
				s += idf * tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(e.n)/avg))
			}
			if s > 0 {
				kw = append(kw, scored{e, s})
			}
		}
		sort.Slice(kw, func(i, j int) bool { return kw[i].s > kw[j].s })
		for rank, x := range kw {
			fused[x.e] += 1 / (rrfK + float64(rank+1))
		}
	}

	// Semantic ranking (cosine similarity).
	if len(qvec) > 0 {
		q := append([]float32(nil), qvec...)
		normalize(q)
		type scored struct {
			e *entry
			s float64
		}
		var sem []scored
		for _, e := range cands {
			if len(e.Vec) == len(q) {
				sem = append(sem, scored{e, dot(e.Vec, q)})
			}
		}
		sort.Slice(sem, func(i, j int) bool { return sem[i].s > sem[j].s })
		for rank, x := range sem {
			if rank >= 50 {
				break
			}
			fused[x.e] += 1 / (rrfK + float64(rank+1))
		}
	}

	out := make([]Result, 0, len(fused))
	for e, s := range fused {
		out = append(out, Result{Piece: e.Piece, Score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].DocID != out[j].DocID {
			return out[i].DocID < out[j].DocID
		}
		return out[i].Seq < out[j].Seq
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by for from has have he her his i if in into is it its
		me my of on or our she so than that the their them then there these they this to was we were what when where
		which who why will with would you your can could do does did not no about how`) {
		stopwords[w] = true
	}
}

// Tokenize lowercases and splits text into search terms.
func Tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(f)) < 2 || stopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
