package catalogstartup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProductionClientUsesSuppliedEndpointAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/overview" {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "claude-opus-5-5")
	}))
	defer server.Close()

	deps := Production(server.URL+"/overview", "https://chatgpt.com/backend-api/codex")
	defer deps.Close()
	if deps.SyntheticAuthClient(20*time.Second) != nil {
		t.Fatal("production Codex credential refresh client changed")
	}
	client := deps.Client(15 * time.Second)
	if client.Timeout != 15*time.Second {
		t.Fatalf("timeout = %s", client.Timeout)
	}
	resp, err := client.Get(deps.OfficialURL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "claude-opus-5-5" {
		t.Fatalf("catalog fetch = %q, %v", body, err)
	}
	if deps.CodexModelsURL() != "https://chatgpt.com/backend-api/codex/models?client_version=0.159.2" {
		t.Fatal("Codex endpoint changed")
	}
}

func TestZeroDependenciesClientUsesDefaultTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "models")
	}))
	defer server.Close()

	var deps Dependencies
	resp, err := deps.Client(2 * time.Second).Get(server.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "models" {
		t.Fatalf("default transport = %q, %v", body, err)
	}
}

func TestCodexClientDoesNotFollowRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			http.Redirect(w, r, "/secret", http.StatusFound)
			return
		}
		t.Error("Codex redirect was followed")
	}))
	defer server.Close()
	deps := Production(server.URL+"/overview", server.URL)
	defer deps.Close()
	client := deps.CodexClient(5 * time.Second)
	if client.Timeout != 5*time.Second {
		t.Fatalf("Codex timeout = %s", client.Timeout)
	}
	resp, err := client.Get(deps.CodexModelsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("Codex redirect status = %d", resp.StatusCode)
	}
}
