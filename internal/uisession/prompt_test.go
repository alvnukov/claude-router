package uisession

import "testing"

func TestFirstPrompt(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"text", `{"messages":[{"role":"assistant","content":"ignore"},{"role":"user","content":"  Найди\nпричину ошибки  "}]}`, "Найди причину ошибки"},
		{"tool and reminder", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"not a title","tool_use_id":"t"},{"type":"text","text":"<system-reminder>Private context</system-reminder>\nИсправь кеш"}]}]}`, "Исправь кеш"},
		{"tool only", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"not a title","tool_use_id":"t"}]}]}`, ""},
		{"command", `{"messages":[{"role":"user","content":"<command-name>/clear</command-name><command-args></command-args>"},{"role":"user","content":"Новая задача"}]}`, "Новая задача"},
		{"malformed", `{"messages":`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstPrompt([]byte(tc.body)); got != tc.want {
				t.Fatalf("title=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestConnection(t *testing.T) {
	for _, tc := range []struct{ route, served, want string }{
		{"cloud", "", "anthropic"},
		{"passthrough", "p/m1", "anthropic"},
		{"local", "p/m1", "p"},
		{"local", "", ""},
	} {
		if got := Connection(tc.route, tc.served); got != tc.want {
			t.Errorf("Connection(%q, %q)=%q want %q", tc.route, tc.served, got, tc.want)
		}
	}
}
