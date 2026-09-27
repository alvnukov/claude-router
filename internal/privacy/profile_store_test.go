package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestProfileSaveRejectsStaleOrInvalidEdits(t *testing.T) {
	home := t.TempDir()
	lab := NewLab(home)
	first := lab.Config()
	if first.Status != "missing" {
		t.Fatal(first.Status)
	}
	body := []byte(`{"version":1,"enabled":true,"default":"base","profiles":[{"id":"base","name":"Base","enabled":true,"rules":{}}],"bindings":[]}`)
	if _, err := lab.SaveConfig(context.Background(), first.Revision, body); err != nil {
		t.Fatal(err)
	}
	next := NewLab(home).Config()
	if next.Status != "ready" || next.Config.Default != "base" {
		t.Fatal("save was not durable")
	}
	if _, err := lab.SaveConfig(context.Background(), first.Revision, body); err == nil {
		t.Fatal("stale edit replaced config")
	}
	if _, err := lab.SaveConfig(context.Background(), next.Revision, []byte(`{"CANARY":"PRIVATE"}`)); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatal("bad validation or unsafe error")
	}
	stored, _ := os.ReadFile(filepath.Join(home, "privacy-profiles.json"))
	if strings.Contains(string(stored), "CANARY") {
		t.Fatal("invalid config saved")
	}
}

func TestProfileSaveReturnsOwnRevisionUnderConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	var wg sync.WaitGroup
	for writer := range 4 {
		wg.Go(func() {
			lab := NewLab(home)
			for attempt := range 20 {
				current := lab.Config()
				name := fmt.Sprintf("writer-%d-%d", writer, attempt)
				body := []byte(fmt.Sprintf(`{"version":1,"enabled":true,"default":"x","profiles":[{"id":"x","name":%q,"enabled":true,"rules":{}}]}`, name))
				saved, err := lab.SaveConfig(context.Background(), current.Revision, body)
				if err != nil {
					if err.Error() == "conflict" {
						continue
					}
					t.Error(err)
					return
				}
				if saved.Status != "ready" || saved.Config.Profiles[0].Name != name {
					t.Error("save returned another writer's config")
					return
				}
				encoded, _ := json.MarshalIndent(saved.Config, "", "  ")
				sum := sha256.Sum256(encoded)
				if saved.Revision != hex.EncodeToString(sum[:]) {
					t.Error("revision does not belong to returned config")
					return
				}
			}
		})
	}
	wg.Wait()
}
