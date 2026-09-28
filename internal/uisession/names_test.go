package uisession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamesSnapshotReadsJournalInBackground(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"type":"custom-title","sessionId":"session-a","customTitle":"Разобрать кеш"}
{"type":"user","sessionId":"session-a","cwd":"/work/router","gitBranch":"main","message":{"role":"user","content":"secret text"}}
{"type":"custom-title","sessionId":"other","customTitle":"Чужое название"}
`
	if err := os.WriteFile(filepath.Join(project, "session-a.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var names Names
	if got := names.Snapshot(root, []string{"session-a"}); got["session-a"] != (Identity{}) {
		t.Fatalf("first snapshot must not wait for journals: %+v", got)
	}
	names.Wait()
	got := names.Snapshot(root, []string{"session-a"})["session-a"]
	if got != (Identity{Title: "Разобрать кеш", Project: "router", Branch: "main"}) {
		t.Fatalf("identity=%+v", got)
	}
}

func TestReadIdentitySkipsPayloadsAndForeignRecords(t *testing.T) {
	input := `{"type":"custom-title","sessionId":"s","customTitle":"Старое"}` + "\n" +
		strings.Repeat("x", (1<<20)+100) + "\n" +
		`{"type":"user","sessionId":"s","cwd":"/work/my-project","gitBranch":"feature/name"}` + "\n" +
		`{"type":"custom-title","sessionId":"s","customTitle":"  Новое\nназвание\u202e  "}` + "\n" +
		`{"type":"custom-title","sessionId":"other","customTitle":"Чужое"}`
	got := readIdentity(strings.NewReader(input), "s")
	if got.Title != "Новое название" || got.Project != "my-project" || got.Branch != "feature/name" {
		t.Fatalf("identity=%+v", got)
	}
}

func TestLoadNamesRefreshesChangedFilesAndContainsPaths(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"custom-title","sessionId":"s","customTitle":"First"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := loadNames(root, []string{"s"}, nil)
	if first["s"].identity.Title != "First" {
		t.Fatalf("first=%+v", first)
	}
	same := loadNames(root, []string{"s"}, first)
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
	renamed := loadNames(root, []string{"s"}, first)
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
		if got := loadNames(root, []string{"escape"}, nil); len(got) != 0 {
			t.Fatalf("read outside projects: %+v", got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := loadNames(root, []string{"s"}, renamed); len(got) != 0 {
		t.Fatalf("deleted journal retained: %+v", got)
	}
}
