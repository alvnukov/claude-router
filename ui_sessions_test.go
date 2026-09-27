package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
)

func TestUISessionNamesFromClaudeHistory(t *testing.T) {
	u, h := testUI(t, "session-a")
	project := filepath.Join(filepath.Dir(u.claudeProxy.path), "projects", "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "session-a.jsonl")
	data := `{"type":"custom-title","sessionId":"session-a","customTitle":"Разобрать кеш"}
{"type":"user","sessionId":"session-a","cwd":"/work/router","gitBranch":"main"}
{"type":"custom-title","sessionId":"other","customTitle":"Чужое название"}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var state struct {
			Sessions []struct{ Title, Project, Branch string }
		}
		if err := json.Unmarshal(apiCall(t, h, "GET", "/api/ui/state", nil).Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if len(state.Sessions) == 1 && state.Sessions[0].Title == "Разобрать кеш" {
			if state.Sessions[0].Project != "router" || state.Sessions[0].Branch != "main" {
				t.Fatalf("identity=%+v", state.Sessions[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing readable session identity: %+v", state.Sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSessionIdentityReaderSkipsPayloadsAndForeignRecords(t *testing.T) {
	input := `{"type":"custom-title","sessionId":"s","customTitle":"Старое"}` + "\n" +
		strings.Repeat("x", (1<<20)+100) + "\n" +
		`{"type":"user","sessionId":"s","cwd":"/work/my-project","gitBranch":"feature/name"}` + "\n" +
		`{"type":"custom-title","sessionId":"s","customTitle":"  Новое\nназвание\u202e  "}` + "\n" +
		`{"type":"custom-title","sessionId":"other","customTitle":"Чужое"}`
	got := readSessionIdentity(strings.NewReader(input), "s")
	if got.Title != "Новое название" || got.Project != "my-project" || got.Branch != "feature/name" {
		t.Fatalf("identity=%+v", got)
	}
}

func TestSessionNameCacheRefreshesChangedFilesAndContainsPaths(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"custom-title","sessionId":"s","customTitle":"First"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := loadSessionNames(root, []string{"s"}, nil)
	if first["s"].identity.Title != "First" {
		t.Fatalf("first=%+v", first)
	}
	same := loadSessionNames(root, []string{"s"}, first)
	if same["s"].stat != first["s"].stat {
		t.Fatal("unchanged journal was reparsed")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"type":"custom-title","sessionId":"s","customTitle":"Renamed"}` + "\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	renamed := loadSessionNames(root, []string{"s"}, first)
	if renamed["s"].identity.Title != "Renamed" {
		t.Fatalf("renamed=%+v", renamed)
	}
	for _, id := range []string{"../s", "../../s", "*", "s/file", `s\file`, ""} {
		if safeSessionID(id) {
			t.Errorf("unsafe id accepted: %q", id)
		}
	}
	outside := filepath.Join(t.TempDir(), "external.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"custom-title","sessionId":"escape","customTitle":"Outside"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "escape.jsonl")); err == nil {
		if got := loadSessionNames(root, []string{"escape"}, nil); len(got) != 0 {
			t.Fatalf("read outside projects: %+v", got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := loadSessionNames(root, []string{"s"}, renamed); len(got) != 0 {
		t.Fatalf("deleted journal retained: %+v", got)
	}
}

func TestFirstSessionPrompt(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"text", `{"messages":[{"role":"assistant","content":"ignore"},{"role":"user","content":"  Найди\nпричину ошибки  "}]}`, "Найди причину ошибки"},
		{"tool and reminder", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"not a title","tool_use_id":"t"},{"type":"text","text":"<system-reminder>Private context</system-reminder>\nИсправь кеш"}]}]}`, "Исправь кеш"},
		{"tool only", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"not a title","tool_use_id":"t"}]}]}`, ""},
		{"command", `{"messages":[{"role":"user","content":"<command-name>/clear</command-name><command-args></command-args>"},{"role":"user","content":"Новая задача"}]}`, "Новая задача"},
		{"malformed", `{"messages":`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstSessionPrompt([]byte(tc.body)); got != tc.want {
				t.Fatalf("title=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestUISessionPromptUsesFirstMeaningfulRequest(t *testing.T) {
	u, h := testUI(t)
	for _, body := range []string{
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"service"}]}]}`,
		`{"messages":[{"role":"user","content":"Первая задача"}]}`,
		`{"messages":[{"role":"user","content":"Поздний вопрос"}]}`,
	} {
		u.st.Add(&history.Record{Start: time.Now(), Model: "claude-sonnet-5", Session: "prompt-session", Route: "cloud", ReqBody: []byte(body)})
	}
	var state struct{ Sessions []struct{ Title string } }
	if err := json.Unmarshal(apiCall(t, h, "GET", "/api/ui/state", nil).Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].Title != "Первая задача" {
		t.Fatalf("titles=%+v", state.Sessions)
	}
}
