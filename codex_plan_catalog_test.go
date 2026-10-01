package main

import (
	"context"
	"encoding/json"
	"localrouter/internal/catalogstartup"
	conf "localrouter/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChatGPTPlanCatalogRefreshAndPersistence(t *testing.T) {
	useTestCodexHome(t, "https://auth.openai.com", http.DefaultClient)
	u, _ := codexUI(t)
	c := planFixture(t, "https://auth.openai.com", true, time.Now().Add(time.Hour))
	if err := codexAuth.save(c); err != nil {
		t.Fatal(err)
	}
	overview := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`<button>claude-opus-5-5</button>`)) }))
	defer overview.Close()
	deps := catalogstartup.Production(overview.URL, conf.CodexBaseURL)
	defer deps.Close()
	failed := false
	hits := 0
	deps.SetCodexTestTransport(usageTransport(func(r *http.Request) (*http.Response, error) {
		hits++
		if r.URL.String() != "https://api.openai.com/v1/models" || r.Header.Get("Authorization") != "Bearer opaque-old" || r.Header.Get("ChatGPT-Account-Id") != "" {
			t.Errorf("wrong catalog request %s", r.URL)
		}
		if failed {
			return usageResponse(503, `{}`), nil
		}
		return usageResponse(200, `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT 6.1 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]},{"slug":"gpt-6-sol","display_name":"GPT 6 Sol","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}`), nil
	}))
	u.catalog = &deps
	provPath := filepath.Join(t.TempDir(), "providers.json")
	u.cs = conf.NewStore(u.cs.Get(), provPath)
	if err := u.refreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	cat := u.cs.Get().Local.Catalog.Providers["codex"]
	if len(cat.Models) != 2 || cat.Models[0].ID != "gpt-6.1-sol" || cat.Models[0].Efforts[0] != "high" || cat.UpdatedAt.IsZero() {
		t.Fatalf("new account model lost: %+v", cat)
	}
	raw, err := os.ReadFile(provPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if json.Unmarshal(raw, &persisted) != nil {
		t.Fatal("catalog not persisted")
	}
	failed = true
	if err = u.refreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	retained := u.cs.Get().Local.Catalog.Providers["codex"]
	if hits != 2 || len(retained.Models) != 2 || retained.Models[0].ID != "gpt-6.1-sol" || retained.Error == "" {
		t.Fatal("failed refresh discarded last successful catalog")
	}
}
