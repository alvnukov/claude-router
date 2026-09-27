package catalogstartup

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

type blockedTransport struct{ calls int }

func (s *blockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls++
	return nil, errors.New("unexpected test transport call")
}

func TestCodexClientKeepsProductionTransportWithNilTestTransport(t *testing.T) {
	deps := Production("https://platform.claude.com/docs/en/models/overview", "https://chatgpt.com/backend-api/codex")
	defer deps.Close()
	original := deps.transport
	deps.SetCodexTestTransport(nil)
	client := deps.CodexClient(5 * time.Second)
	if client.Transport != original {
		t.Fatal("nil test transport replaced the production transport")
	}
	if client.CheckRedirect == nil {
		t.Fatal("Codex redirect policy was lost")
	}
}
