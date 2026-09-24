package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentAuthImportsLeaveOneValidCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.json")
	stores := []*codexAuthStore{{path: path, cliPath: filepath.Join(dir, "one.json")}, {path: path, cliPath: filepath.Join(dir, "two.json")}}
	for i, s := range stores {
		c := usageCredential("account")
		c.Tokens.RefreshToken = []string{"one", "two"}[i]
		c.Tokens.AccessToken = testJWT(time.Now().Add(time.Hour))
		data, _ := json.Marshal(c)
		if err := os.WriteFile(s.cliPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.importFromCLI(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := readCodexCredential(path)
	if err != nil || (got.Tokens.RefreshToken != "one" && got.Tokens.RefreshToken != "two") {
		t.Fatalf("invalid concurrent login: %v %+v", err, got)
	}
}

func TestQuiescedAuthRefusesImport(t *testing.T) {
	dir := t.TempDir()
	s := &codexAuthStore{path: filepath.Join(dir, "auth.json"), cliPath: filepath.Join(dir, "cli.json"), life: newLifecycle(false)}
	c := usageCredential("account")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(s.cliPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.life.quiesce(); err != nil {
		t.Fatal(err)
	}
	if err := s.importFromCLI(); err == nil {
		t.Fatal("quiesced instance imported credential")
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatalf("quiesced instance wrote auth: %v", err)
	}
}
