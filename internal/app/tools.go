package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ninovee/usbai/internal/llama"
)

// Agent tools run locally and never touch the network or the host's files.
// Each one is deliberately small and safe: the riskiest can only read the
// user's own vault or add a note or memory to it.

type toolSpec struct {
	Label       string // shown in the UI
	Description string // shown to the model
	Params      map[string]string
	Required    []string
}

var toolCatalog = map[string]toolSpec{
	"search_documents": {
		Label:       "Search documents",
		Description: "Search the user's documents for passages relevant to a query. Returns the best matching excerpts with document names.",
		Params:      map[string]string{"query": "what to search for"},
		Required:    []string{"query"},
	},
	"read_document": {
		Label:       "Read a document",
		Description: "Read the full text of one of the user's documents by name (or part of its name).",
		Params:      map[string]string{"name": "document name, e.g. contract.pdf"},
		Required:    []string{"name"},
	},
	"list_documents": {
		Label:       "List documents",
		Description: "List the names of the user's documents.",
	},
	"calculator": {
		Label:       "Calculator",
		Description: "Evaluate an arithmetic expression exactly. Supports + - * / % ^, parentheses, and sqrt, abs, round, floor, ceil, min, max.",
		Params:      map[string]string{"expression": "e.g. (1200 * 12) * 1.015"},
		Required:    []string{"expression"},
	},
	"get_datetime": {
		Label:       "Date & time",
		Description: "Get the current local date, time and weekday.",
	},
	"remember": {
		Label:       "Remember facts",
		Description: "Save a lasting fact or preference about the user to memory, so every future conversation knows it.",
		Params:      map[string]string{"fact": "the fact to remember, in one sentence"},
		Required:    []string{"fact"},
	},
	"web_search": {
		Label:       "🌐 Search the web",
		Description: "Search the internet. Returns titles, addresses and snippets of the top results. Uses the internet: only the query is sent.",
		Params:      map[string]string{"query": "what to search for"},
		Required:    []string{"query"},
	},
	"read_webpage": {
		Label:       "🌐 Read web pages",
		Description: "Read the text of a web page by its address (from search results or the user). Uses the internet.",
		Params:      map[string]string{"url": "the page address, e.g. https://example.com/article"},
		Required:    []string{"url"},
	},
	"create_note": {
		Label:       "Create notes",
		Description: "Save a note as a new document in the user's Files (Markdown). Use for summaries, plans or anything the user wants to keep.",
		Params:      map[string]string{"title": "short title", "content": "the note in Markdown"},
		Required:    []string{"title", "content"},
	},
}

// ToolInfo describes a tool for the UI.
type ToolInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ToolInfos lists the tools in a stable order.
func ToolInfos() []ToolInfo {
	var out []ToolInfo
	for id, t := range toolCatalog {
		out = append(out, ToolInfo{ID: id, Label: t.Label, Description: t.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// toolDefs builds the function definitions sent to the model.
func toolDefs(names []string) []llama.Tool {
	var out []llama.Tool
	for _, n := range names {
		spec, ok := toolCatalog[n]
		if !ok {
			continue
		}
		props := map[string]any{}
		for p, desc := range spec.Params {
			props[p] = map[string]string{"type": "string", "description": desc}
		}
		required := spec.Required
		if required == nil {
			required = []string{}
		}
		out = append(out, llama.Tool{Type: "function", Function: llama.ToolFunction{
			Name:        n,
			Description: spec.Description,
			Parameters:  map[string]any{"type": "object", "properties": props, "required": required},
		}})
	}
	return out
}

const toolResultLimit = 6000 // characters returned to the model per call

// runTool executes one tool call for an agent and returns the text result
// given back to the model. Errors are returned as text too, so the model can
// recover (e.g. retry with a different document name).
func (a *App) runTool(ag *Agent, call llama.ToolCall) string {
	if !ag.HasTool(call.Function.Name) {
		return "Error: tool " + call.Function.Name + " is not enabled for this agent."
	}
	args := map[string]any{}
	if s := strings.TrimSpace(call.Function.Arguments); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return "Error: arguments must be a JSON object: " + err.Error()
		}
	}
	arg := func(k string) string {
		v, _ := args[k].(string)
		if v == "" && args[k] != nil {
			v = fmt.Sprint(args[k])
		}
		return strings.TrimSpace(v)
	}

	var out string
	var err error
	switch call.Function.Name {
	case "search_documents":
		out, err = a.toolSearch(ag, arg("query"))
	case "read_document":
		out, err = a.toolRead(ag, arg("name"))
	case "list_documents":
		out, err = a.toolList(ag)
	case "calculator":
		var v float64
		if v, err = Calculate(arg("expression")); err == nil {
			out = formatNumber(v)
		}
	case "get_datetime":
		out = time.Now().Format("Monday, 2 January 2006, 15:04 MST")
	case "remember":
		if _, err = a.Remember(arg("fact")); err == nil {
			out = "Saved to memory."
		}
	case "web_search":
		var results []SearchResult
		var provider string
		if results, provider, err = a.WebSearch(a.ctx, arg("query")); err == nil {
			out = formatSearch(results, provider)
		} else {
			// Small models otherwise retry the same failing search.
			return "Error: " + err.Error() + "\nThe web search is not working right now, so searching again will fail too." +
				" If you have a web address (for example one the user gave), read it with read_webpage instead." +
				" Otherwise tell the user the search failed and why, then answer from what you know, saying it may be out of date."
		}
	case "read_webpage":
		var page string
		if page, err = a.ReadWebpage(a.ctx, arg("url")); err == nil {
			out = untrustedLabel + page
		} else {
			return "Error: " + err.Error() + "\nThis page could not be read. Do not describe or cite its contents; say it could not be opened."
		}
	case "create_note":
		title := arg("title")
		if title == "" {
			title = "Note " + time.Now().Format("2006-01-02 15:04")
		}
		name := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(title) + ".md"
		var meta DocMeta
		if meta, err = a.AddDocument(name, []byte("# "+title+"\n\n"+arg("content"))); err == nil {
			out = "Saved note " + meta.Name + " to Files."
		}
	default:
		err = errors.New("unknown tool")
	}
	if err != nil {
		return "Error: " + err.Error()
	}
	if r := []rune(out); len(r) > toolResultLimit {
		out = string(r[:toolResultLimit]) + "\n…(truncated)"
	}
	return out
}

func (a *App) scopedDocs(ag *Agent) []DocMeta {
	docs, _ := a.Documents()
	scope := ag.docScope()
	if scope == nil {
		return docs
	}
	var out []DocMeta
	for _, d := range docs {
		if scope[d.ID] {
			out = append(out, d)
		}
	}
	return out
}

func (a *App) toolList(ag *Agent) (string, error) {
	docs := a.scopedDocs(ag)
	if len(docs) == 0 {
		return "No documents available.", nil
	}
	var b strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&b, "- %s (%d characters, added %s)\n", d.Name, d.Chars, d.Added.Format("2 Jan 2006"))
	}
	return b.String(), nil
}

func (a *App) toolSearch(ag *Agent, query string) (string, error) {
	if query == "" {
		return "", errors.New("query is empty")
	}
	docs := a.scopedDocs(ag)
	if len(docs) == 0 {
		return "No documents available to search.", nil
	}
	filter := map[string]bool{}
	for _, d := range docs {
		filter[d.ID] = true
	}
	a.dataMu.RLock()
	index := a.index
	a.dataMu.RUnlock()
	if index == nil {
		return "", errLocked
	}
	var qvec []float32
	if _, em, ready := a.embedEngine(); ready {
		if vecs, ok, err := a.embedTexts([]string{query}, em.QueryPrefix); err == nil && ok {
			qvec = vecs[0]
		}
	}
	results := index.Search(query, qvec, retrievalTopK, filter)
	if len(results) == 0 {
		return "No matching passages found.", nil
	}
	text, _ := formatExcerpts(results, toolResultLimit/4)
	return text, nil
}

func (a *App) toolRead(ag *Agent, name string) (string, error) {
	docs := a.scopedDocs(ag)
	want := strings.ToLower(name)
	var match *DocMeta
	for i := range docs {
		if strings.ToLower(docs[i].Name) == want {
			match = &docs[i]
			break
		}
	}
	if match == nil {
		for i := range docs {
			if want != "" && strings.Contains(strings.ToLower(docs[i].Name), want) {
				match = &docs[i]
				break
			}
		}
	}
	if match == nil {
		list, _ := a.toolList(ag)
		return "", fmt.Errorf("no document named %q. Available:\n%s", name, list)
	}
	v, err := a.unlocked()
	if err != nil {
		return "", err
	}
	text, err := v.Get("docs/" + match.ID + "/text")
	if err != nil {
		return "", err
	}
	return "[Document: " + match.Name + "]\n" + string(text), nil
}

var thousandsSep = regexp.MustCompile(`(\d),(\d{3})\b`)

func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'g', 12, 64)
}

// Calculate evaluates an arithmetic expression with a small recursive-descent
// parser. It never executes code.
func Calculate(expr string) (float64, error) {
	expr = strings.NewReplacer("×", "*", "÷", "/", "−", "-").Replace(expr)
	expr = thousandsSep.ReplaceAllString(expr, "$1$2") // 1,200 -> 1200; keeps f(a, b)
	p := &calcParser{s: expr}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	p.skip()
	if p.i < len(p.s) {
		return 0, fmt.Errorf("unexpected %q", p.s[p.i:])
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("result is not a finite number")
	}
	return v, nil
}

type calcParser struct {
	s     string
	i     int
	depth int
}

func (p *calcParser) skip() {
	for p.i < len(p.s) && unicode.IsSpace(rune(p.s[p.i])) {
		p.i++
	}
}

func (p *calcParser) peek() byte {
	p.skip()
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

// expr := term (('+'|'-') term)*
func (p *calcParser) expr() (float64, error) {
	if p.depth++; p.depth > 100 {
		return 0, errors.New("expression too deeply nested")
	}
	defer func() { p.depth-- }()
	v, err := p.term()
	for err == nil {
		switch p.peek() {
		case '+':
			p.i++
			var r float64
			r, err = p.term()
			v += r
		case '-':
			p.i++
			var r float64
			r, err = p.term()
			v -= r
		default:
			return v, nil
		}
	}
	return 0, err
}

// term := power (('*'|'/'|'%') power)*
func (p *calcParser) term() (float64, error) {
	v, err := p.power()
	for err == nil {
		op := p.peek()
		if op != '*' && op != '/' && op != '%' {
			return v, nil
		}
		p.i++
		var r float64
		if r, err = p.power(); err != nil {
			break
		}
		switch op {
		case '*':
			v *= r
		case '/':
			if r == 0 {
				return 0, errors.New("division by zero")
			}
			v /= r
		case '%':
			if r == 0 {
				return 0, errors.New("division by zero")
			}
			v = math.Mod(v, r)
		}
	}
	return 0, err
}

// power := unary ('^' power)?
func (p *calcParser) power() (float64, error) {
	v, err := p.unary()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.i++
		e, err := p.power()
		if err != nil {
			return 0, err
		}
		return math.Pow(v, e), nil
	}
	return v, nil
}

// unary := ('-'|'+') unary | primary
func (p *calcParser) unary() (float64, error) {
	switch p.peek() {
	case '-':
		p.i++
		v, err := p.unary()
		return -v, err
	case '+':
		p.i++
		return p.unary()
	}
	return p.primary()
}

// primary := number | '(' expr ')' | func '(' args ')'
func (p *calcParser) primary() (float64, error) {
	c := p.peek()
	switch {
	case c == '(':
		p.i++
		v, err := p.expr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, errors.New("missing )")
		}
		p.i++
		return v, nil
	case c >= '0' && c <= '9' || c == '.':
		start := p.i
		for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '.' || p.s[p.i] == 'e' || p.s[p.i] == 'E' ||
			(p.i > start && (p.s[p.i] == '-' || p.s[p.i] == '+') && (p.s[p.i-1] == 'e' || p.s[p.i-1] == 'E'))) {
			p.i++
		}
		v, err := strconv.ParseFloat(p.s[start:p.i], 64)
		if err != nil {
			return 0, fmt.Errorf("bad number %q", p.s[start:p.i])
		}
		return v, nil
	case unicode.IsLetter(rune(c)):
		start := p.i
		for p.i < len(p.s) && unicode.IsLetter(rune(p.s[p.i])) {
			p.i++
		}
		name := strings.ToLower(p.s[start:p.i])
		switch name {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		}
		if p.peek() != '(' {
			return 0, fmt.Errorf("unknown name %q", name)
		}
		p.i++
		var args []float64
		for p.peek() != ')' {
			v, err := p.expr()
			if err != nil {
				return 0, err
			}
			args = append(args, v)
			if p.peek() == ',' {
				p.i++
			} else if p.peek() != ')' {
				return 0, errors.New("missing )")
			}
		}
		p.i++
		return callFunc(name, args)
	}
	if c == 0 {
		return 0, errors.New("unexpected end of expression")
	}
	return 0, fmt.Errorf("unexpected %q", string(c))
}

func callFunc(name string, args []float64) (float64, error) {
	one := func(f func(float64) float64) (float64, error) {
		if len(args) != 1 {
			return 0, fmt.Errorf("%s takes 1 argument", name)
		}
		return f(args[0]), nil
	}
	switch name {
	case "sqrt":
		return one(math.Sqrt)
	case "abs":
		return one(math.Abs)
	case "floor":
		return one(math.Floor)
	case "ceil":
		return one(math.Ceil)
	case "ln", "log":
		return one(math.Log)
	case "log10":
		return one(math.Log10)
	case "round":
		if len(args) == 2 {
			m := math.Pow(10, args[1])
			return math.Round(args[0]*m) / m, nil
		}
		return one(math.Round)
	case "min", "max":
		if len(args) == 0 {
			return 0, fmt.Errorf("%s needs arguments", name)
		}
		v := args[0]
		for _, x := range args[1:] {
			if name == "min" {
				v = math.Min(v, x)
			} else {
				v = math.Max(v, x)
			}
		}
		return v, nil
	}
	return 0, fmt.Errorf("unknown function %q", name)
}
