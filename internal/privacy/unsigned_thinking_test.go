package privacy

import (
	"bytes"
	"testing"
)

func TestDropUnsignedThinking(t *testing.T) {
	const (
		on       = `"thinking":{"type":"enabled","budget_tokens":16,"display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`
		signed   = `{"type":"thinking","thinking":"claude","signature":"sig"}`
		unsigned = `{"type":"thinking","thinking":"codex"}`
		toolUse  = `{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}`
		result   = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}`
		result2  = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"b"}]}`
		codex    = `{"role":"user","content":"run"},{"role":"assistant","content":[` + unsigned + `,` + toolUse + `]},` + result
		cleaned  = `{"role":"user","content":"run"},{"role":"assistant","content":[` + toolUse + `]},` + result
		more     = `,{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{}}]},` + result2
		old      = `{"role":"user","content":"a"},{"role":"assistant","content":[` + unsigned + `,{"type":"text","text":"b"}]}`
		oldClean = `{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"text","text":"b"}]}`
	)
	body := func(controls, messages string) string {
		if controls != "" {
			controls += ","
		}
		return `{"model":"m","max_tokens":100,` + controls + `"messages":[` + messages + `]}`
	}
	for _, c := range []struct{ name, in, want string }{
		{"no thinking", body("", `{"role":"user","content":"hi"}`), ""},
		{"signed and redacted kept", body(on, `{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"a"}]},{"role":"user","content":"next"}`), ""},
		{"user blocks are not ours", body("", `{"role":"user","content":[`+unsigned+`]}`), ""},
		{"missing, null and empty signatures", body("", `{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"thinking","thinking":"a"}, {"type":"thinking","thinking":"b","signature":null},{"type":"thinking","thinking":"c","signature":""},{ "type" : "text", "text":"a " }]},{"role":"user","content":"b"}`),
			body("", `{"role":"user","content":"a"},{"role":"assistant","content":[{ "type" : "text", "text":"a " }]},{"role":"user","content":"b"}`)},
		{"thinking-only message deleted", body("", `{"role":"user","content":"a"},{"role":"assistant","content":[`+unsigned+`]},{"role":"user","content":"b"}`),
			body("", `{"role":"user","content":"a"},{"role":"user","content":"b"}`)},
		{"open turn after Codex tool_use keeps thinking", body(on, codex), body(on, cleaned)},
		{"open turn before trailing system keeps thinking", body(on, codex+`,{"role":"system","content":"s"}`), body(on, cleaned+`,{"role":"system","content":"s"}`)},
		{"open turn continues", body(on, codex+more), body(on, cleaned+more)},
		{"Codex tool_use without thinking goes as is", body(on, cleaned), ""},
		{"new turn keeps thinking", body(on, codex+`,{"role":"assistant","content":[{"type":"text","text":"done"}]},{"role":"user","content":"next"}`),
			body(on, cleaned+`,{"role":"assistant","content":[{"type":"text","text":"done"}]},{"role":"user","content":"next"}`)},
		{"normal turn keeps thinking", body(on, old+`,{"role":"user","content":"go"},{"role":"assistant","content":[`+signed+`,`+toolUse+`]},`+result+more),
			body(on, oldClean+`,{"role":"user","content":"go"},{"role":"assistant","content":[`+signed+`,`+toolUse+`]},`+result+more)},
		{"unsigned block mid-turn", body(on, `{"role":"user","content":"go"},{"role":"assistant","content":[`+signed+`,`+toolUse+`]},`+result+`,{"role":"assistant","content":[`+unsigned+`,{"type":"tool_use","id":"t2","name":"Bash","input":{}}]},`+result2),
			body(on, `{"role":"user","content":"go"},{"role":"assistant","content":[`+signed+`,`+toolUse+`]},`+result+more)},
		{"adaptive untouched", body(`"thinking":{"type":"adaptive"}`, codex), body(`"thinking":{"type":"adaptive"}`, cleaned)},
		{"enabled without context_management", body(`"thinking":{"type":"enabled","budget_tokens":16}`, codex), body(`"thinking":{"type":"enabled","budget_tokens":16}`, cleaned)},
		{"context_management first", `{"context_management":{"edits":[]},"thinking":{"type":"enabled","budget_tokens":16},"messages":[` + codex + `]}`, `{"context_management":{"edits":[]},"thinking":{"type":"enabled","budget_tokens":16},"messages":[` + cleaned + `]}`},
		{"invalid JSON", `{"messages":[` + unsigned, ""},
		{"not an object", `[` + unsigned + `]`, ""},
		{"duplicate key", `{"messages":[],"messages":[{"role":"assistant","content":[` + unsigned + `]}]}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.in)
			got := DropUnsignedThinking(in)
			if c.want == "" {
				if &got[0] != &in[0] || len(got) != len(in) {
					t.Fatalf("changed a body it had no business with:\n%s", got)
				}
				return
			}
			if string(got) != c.want {
				t.Fatalf("got\n%s\nwant\n%s", got, c.want)
			}
			if again := DropUnsignedThinking(got); !bytes.Equal(again, got) {
				t.Fatalf("not idempotent:\n%s", again)
			}
			if twice := DropUnsignedThinking([]byte(c.in)); !bytes.Equal(twice, got) {
				t.Fatalf("not deterministic:\n%s", twice)
			}
		})
	}
	t.Run("earlier messages are a prefix", func(t *testing.T) {
		short := DropUnsignedThinking([]byte(body(on, old+`,{"role":"user","content":"go"}`)))
		long := DropUnsignedThinking([]byte(body(on, old+`,{"role":"user","content":"go"},{"role":"assistant","content":[`+unsigned+`,{"type":"text","text":"c"}]},{"role":"user","content":"d"}`)))
		prefix := short[:bytes.LastIndex(short, []byte("]}"))]
		if !bytes.HasPrefix(long, prefix) {
			t.Fatalf("history changed under a new turn:\n%s\n%s", short, long)
		}
	})
}
