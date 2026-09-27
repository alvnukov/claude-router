//go:build router_codex_loopback && !catalogsynthetic

package catalogstartup

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"localrouter/internal/codextesttransport"
)

func TestLoopbackCodexModelsRejectsBeforeInjectedDial(t *testing.T) {
	deps := Production("https://platform.claude.com/docs/en/models/overview", "https://chatgpt.com/backend-api/codex")
	defer deps.Close()
	calls := 0
	deps.transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		calls++
		return nil, errors.New("unexpected production dial")
	}}
	deps.SetCodexTestTransport(codextesttransport.AuthTransport())
	_, msg := deps.FetchModels(context.Background(), deps.CodexModelsURL(), ProbeInput{
		Name: "codex", Kind: "codex", BaseURL: "https://chatgpt.com/backend-api/codex",
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer RR_SYNTHETIC.eyJleHAiOjQxMDI0NDQ4MDB9.sig")
		req.Header.Set("ChatGPT-Account-Id", "RR_SYNTHETIC_ACCOUNT")
		return nil
	})
	if !strings.Contains(msg, "synthetic Codex transport rejected request") || calls != 0 {
		t.Fatalf("tagged models request escaped the rejecting transport: %q, dials=%d", msg, calls)
	}
	if deps.CodexClient(5*time.Second).CheckRedirect == nil {
		t.Fatal("tagged Codex redirect policy was lost")
	}
}
