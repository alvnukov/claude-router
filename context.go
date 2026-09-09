package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"regexp"
	"sort"
	"strings"
)

// The structure view. A request is shown as what it is made of -- system
// blocks, tool definitions, a timeline of messages, each message a list of
// typed blocks -- with sizes everywhere, so the shape of the context is visible
// before any of the text is read.

var kindOrder = []string{"system", "tools", "user", "assistant", "tool_use", "tool_result", "image", "thinking", "other"}

type blockView struct {
	Kind      string
	Label     string
	HTML      template.HTML
	Chars     int
	Cache     string
	Name      string
	ID        string
	ToolUseID string
	IsError   bool
	Anchor    string
	Matches   int
	Children  []blockView
	Note      string
}

type msgView struct {
	Index   int
	Role    string
	Chars   int
	Pct     float64
	Blocks  []blockView
	Anchor  string
	Matches int
	Summary string
	Cache   bool
}

type kindStat struct {
	Kind  string
	Chars int
	Pct   float64
	Count int
}

type toolView struct {
	Name    string
	Desc    template.HTML
	Schema  template.HTML
	Chars   int
	Matches int
	Anchor  string
}

type ctxView struct {
	Params     []kv
	Headers    []kv
	System     []blockView
	SystemN    int
	Tools      []toolView
	ToolsN     int
	Messages   []msgView
	Kinds      []kindStat
	TotalChars int
	Matches    int
	ParseErr   string
	CacheMarks int
}

type ctxBuilder struct {
	re    *regexp.Regexp
	kinds map[string]*kindStat
	v     *ctxView
}

func (b *ctxBuilder) count(kind string, n int) {
	k, ok := b.kinds[kind]
	if !ok {
		k = &kindStat{Kind: kind}
		b.kinds[kind] = k
	}
	k.Chars += n
	k.Count++
	b.v.TotalChars += n
}

func (b *ctxBuilder) text(kind, label, text, anchor string) blockView {
	bv := blockView{Kind: kind, Label: label, HTML: highlight(text, b.re), Chars: len(text),
		Anchor: anchor, Matches: countMatches(text, b.re)}
	b.count(kind, len(text))
	b.v.Matches += bv.Matches
	return bv
}

func (b *ctxBuilder) finish() *ctxView {
	for _, k := range kindOrder {
		if s, ok := b.kinds[k]; ok {
			if b.v.TotalChars > 0 {
				s.Pct = 100 * float64(s.Chars) / float64(b.v.TotalChars)
			}
			b.v.Kinds = append(b.v.Kinds, *s)
		}
	}
	for i := range b.v.Messages {
		if b.v.TotalChars > 0 {
			b.v.Messages[i].Pct = 100 * float64(b.v.Messages[i].Chars) / float64(b.v.TotalChars)
		}
	}
	return b.v
}

// buildContext renders an Anthropic /v1/messages body.
func buildContext(body []byte, re *regexp.Regexp) *ctxView {
	b := &ctxBuilder{re: re, kinds: map[string]*kindStat{}, v: &ctxView{}}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		b.v.ParseErr = err.Error()
		return b.finish()
	}

	// Scalars and small objects go to the parameter table, in a stable order.
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "system", "messages", "tools":
			continue
		}
		val := string(top[k])
		if len(val) > 300 {
			val = val[:300] + fmt.Sprintf("… (%d байт)", len(top[k]))
		}
		b.v.Params = append(b.v.Params, kv{k, val})
	}

	// System: a bare string or an array of text blocks with cache markers.
	if raw := top["system"]; len(raw) > 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			b.v.System = append(b.v.System, b.text("system", "system", s, "sys-0"))
		} else {
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(raw, &blocks) == nil {
				for i, blk := range blocks {
					t, _ := jsonString(blk["text"])
					bv := b.text("system", fmt.Sprintf("system[%d]", i), t, fmt.Sprintf("sys-%d", i))
					bv.Cache = cacheMark(blk["cache_control"])
					if bv.Cache != "" {
						b.v.CacheMarks++
					}
					b.v.System = append(b.v.System, bv)
				}
			}
		}
		for _, s := range b.v.System {
			b.v.SystemN += s.Chars
		}
	}

	if raw := top["tools"]; len(raw) > 0 {
		var tools []map[string]json.RawMessage
		json.Unmarshal(raw, &tools)
		for i, t := range tools {
			name, _ := jsonString(t["name"])
			desc, _ := jsonString(t["description"])
			schema := prettyJSON(t["input_schema"])
			n := len(name) + len(desc) + len(t["input_schema"])
			tv := toolView{Name: name, Desc: highlight(desc, re), Schema: highlight(schema, re), Chars: n,
				Anchor: fmt.Sprintf("tool-%d", i), Matches: countMatches(name+"\n"+desc+"\n"+schema, re)}
			b.count("tools", n)
			b.v.Matches += tv.Matches
			b.v.ToolsN += n
			b.v.Tools = append(b.v.Tools, tv)
		}
	}

	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	json.Unmarshal(top["messages"], &msgs)
	for i, m := range msgs {
		mv := msgView{Index: i, Role: m.Role, Anchor: fmt.Sprintf("m-%d", i)}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			mv.Blocks = append(mv.Blocks, b.text(m.Role, "text", s, fmt.Sprintf("m-%d-0", i)))
		} else {
			var blocks []map[string]json.RawMessage
			json.Unmarshal(m.Content, &blocks)
			for j, blk := range blocks {
				mv.Blocks = append(mv.Blocks, b.block(m.Role, blk, fmt.Sprintf("m-%d-%d", i, j)))
			}
		}
		for _, bv := range mv.Blocks {
			mv.Chars += bv.Chars
			mv.Matches += bv.Matches
			if bv.Cache != "" {
				mv.Cache = true
			}
			if mv.Summary == "" {
				mv.Summary = bv.Kind
			} else if !strings.Contains(mv.Summary, bv.Kind) {
				mv.Summary += ", " + bv.Kind
			}
		}
		b.v.Messages = append(b.v.Messages, mv)
	}
	return b.finish()
}

func (b *ctxBuilder) block(role string, blk map[string]json.RawMessage, anchor string) blockView {
	kind, _ := jsonString(blk["type"])
	var bv blockView
	switch kind {
	case "text":
		t, _ := jsonString(blk["text"])
		bv = b.text(role, "text", t, anchor)
	case "thinking", "redacted_thinking":
		t, _ := jsonString(blk["thinking"])
		if kind == "redacted_thinking" {
			t = fmt.Sprintf("[redacted_thinking, %d байт]", len(blk["data"]))
		}
		bv = b.text("thinking", kind, t, anchor)
	case "tool_use":
		in := prettyJSON(blk["input"])
		bv = b.text("tool_use", "tool_use", in, anchor)
		bv.Name, _ = jsonString(blk["name"])
		bv.ID, _ = jsonString(blk["id"])
	case "tool_result":
		bv = blockView{Kind: "tool_result", Label: "tool_result", Anchor: anchor}
		bv.ToolUseID, _ = jsonString(blk["tool_use_id"])
		var isErr bool
		json.Unmarshal(blk["is_error"], &isErr)
		bv.IsError = isErr
		var s string
		if json.Unmarshal(blk["content"], &s) == nil {
			c := b.text("tool_result", "text", s, anchor+"-0")
			bv.Children = append(bv.Children, c)
		} else {
			var inner []map[string]json.RawMessage
			json.Unmarshal(blk["content"], &inner)
			for j, ib := range inner {
				ik, _ := jsonString(ib["type"])
				var c blockView
				if ik == "text" {
					t, _ := jsonString(ib["text"])
					c = b.text("tool_result", "text", t, fmt.Sprintf("%s-%d", anchor, j))
				} else {
					c = b.media(ik, ib, fmt.Sprintf("%s-%d", anchor, j))
				}
				bv.Children = append(bv.Children, c)
			}
		}
		for _, c := range bv.Children {
			bv.Chars += c.Chars
			bv.Matches += c.Matches
		}
	case "image", "document":
		bv = b.media(kind, blk, anchor)
	default:
		raw := prettyJSON(mustMarshal(blk))
		bv = b.text("other", kind, raw, anchor)
	}
	bv.Cache = cacheMark(blk["cache_control"])
	if bv.Cache != "" {
		b.v.CacheMarks++
	}
	return bv
}

func (b *ctxBuilder) media(kind string, blk map[string]json.RawMessage, anchor string) blockView {
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	json.Unmarshal(blk["source"], &src)
	n := len(src.Data) + len(src.URL)
	note := fmt.Sprintf("[%s %s %s, %s base64]", kind, src.Type, src.MediaType, fmtChars(n))
	if src.URL != "" {
		note = fmt.Sprintf("[%s url %s]", kind, src.URL)
	}
	bv := blockView{Kind: "image", Label: kind, Anchor: anchor, Chars: n, Note: note}
	b.count("image", n)
	return bv
}

func cacheMark(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var cc struct {
		Type string `json:"type"`
		TTL  string `json:"ttl"`
	}
	json.Unmarshal(raw, &cc)
	if cc.TTL != "" {
		return cc.Type + " " + cc.TTL
	}
	if cc.Type == "" {
		return "cache"
	}
	return cc.Type
}

// buildOpenAIContext renders the translated chat/completions payload the local
// endpoint received, in the same shape so the two can be compared side by side.
func buildOpenAIContext(body []byte, re *regexp.Regexp) *ctxView {
	b := &ctxBuilder{re: re, kinds: map[string]*kindStat{}, v: &ctxView{}}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		b.v.ParseErr = err.Error()
		return b.finish()
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "messages" || k == "tools" {
			continue
		}
		val := string(top[k])
		if len(val) > 300 {
			val = val[:300] + "…"
		}
		b.v.Params = append(b.v.Params, kv{k, val})
	}

	var tools []openaiTool
	json.Unmarshal(top["tools"], &tools)
	for i, t := range tools {
		schema := prettyJSON(t.Function.Parameters)
		n := len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
		tv := toolView{Name: t.Function.Name, Desc: highlight(t.Function.Description, re),
			Schema: highlight(schema, re), Chars: n, Anchor: fmt.Sprintf("tool-%d", i),
			Matches: countMatches(t.Function.Name+"\n"+t.Function.Description+"\n"+schema, re)}
		b.count("tools", n)
		b.v.Matches += tv.Matches
		b.v.ToolsN += n
		b.v.Tools = append(b.v.Tools, tv)
	}

	var msgs []struct {
		Role       string           `json:"role"`
		Content    json.RawMessage  `json:"content"`
		ToolCalls  []openaiToolCall `json:"tool_calls"`
		ToolCallID string           `json:"tool_call_id"`
	}
	json.Unmarshal(top["messages"], &msgs)
	for i, m := range msgs {
		anchor := fmt.Sprintf("m-%d", i)
		if m.Role == "system" {
			s, _ := jsonString(m.Content)
			bv := b.text("system", "system", s, "sys-"+fmt.Sprint(i))
			b.v.System = append(b.v.System, bv)
			b.v.SystemN += bv.Chars
			continue
		}
		mv := msgView{Index: i, Role: m.Role, Anchor: anchor}
		kind := m.Role
		if m.Role == "tool" {
			kind = "tool_result"
		}
		var s string
		if len(m.Content) > 0 && json.Unmarshal(m.Content, &s) == nil {
			if s != "" {
				bv := b.text(kind, "text", s, anchor+"-0")
				bv.ToolUseID = m.ToolCallID
				mv.Blocks = append(mv.Blocks, bv)
			}
		} else if len(m.Content) > 0 {
			var parts []map[string]json.RawMessage
			json.Unmarshal(m.Content, &parts)
			for j, p := range parts {
				pt, _ := jsonString(p["type"])
				if pt == "text" {
					t, _ := jsonString(p["text"])
					mv.Blocks = append(mv.Blocks, b.text(kind, "text", t, fmt.Sprintf("%s-%d", anchor, j)))
				} else {
					raw := prettyJSON(mustMarshal(p))
					bv := blockView{Kind: "image", Label: pt, Anchor: fmt.Sprintf("%s-%d", anchor, j), Chars: len(raw),
						Note: fmt.Sprintf("[%s, %s]", pt, fmtChars(len(raw)))}
					b.count("image", len(raw))
					mv.Blocks = append(mv.Blocks, bv)
				}
			}
		}
		for j, tc := range m.ToolCalls {
			bv := b.text("tool_use", "tool_call", prettyJSON(json.RawMessage(tc.Function.Arguments)), fmt.Sprintf("%s-tc%d", anchor, j))
			bv.Name = tc.Function.Name
			bv.ID = tc.ID
			mv.Blocks = append(mv.Blocks, bv)
		}
		for _, bv := range mv.Blocks {
			mv.Chars += bv.Chars
			mv.Matches += bv.Matches
			if mv.Summary == "" {
				mv.Summary = bv.Kind
			} else if !strings.Contains(mv.Summary, bv.Kind) {
				mv.Summary += ", " + bv.Kind
			}
		}
		b.v.Messages = append(b.v.Messages, mv)
	}
	return b.finish()
}
