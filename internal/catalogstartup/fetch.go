package catalogstartup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"localrouter/internal/chatgptplan"
	"net/http"
	"time"
)

// FetchOfficial reads the bounded official page; the caller owns HTML parsing.
func (d Dependencies) FetchOfficial(ctx context.Context, fallbackURL string) ([]byte, error) {
	endpoint := d.officialURL
	if endpoint == "" {
		endpoint = fallbackURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "claude-router/1.0")
	req.Header.Set("Accept", "text/html")
	resp, err := d.Client(15 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2<<20 {
		return nil, errors.New("каталог слишком велик")
	}
	return body, nil
}

// ProbeInput carries only the provider details needed for a guarded models request.
type ProbeInput struct {
	Name, Kind, BaseURL, AuthID, APIKey string
	ChatGPTPlan                         bool
}

// FetchModels owns target selection, authorization order and transport policy.
// The caller retains the provider-specific JSON parsing and cache.
func (d Dependencies) FetchModels(ctx context.Context, fallbackCodexURL string, p ProbeInput, authorize func(context.Context, *http.Request) error) ([]byte, string) {
	if err := d.ValidateProvider(p.Name, p.Kind, p.BaseURL, p.AuthID); err != nil {
		return nil, err.Error()
	}
	endpoint := p.BaseURL + "/models"
	if p.Kind == "codex" {
		endpoint = d.codexURL
		if endpoint == "" {
			endpoint = fallbackCodexURL
		}
		if p.ChatGPTPlan {
			endpoint = chatgptplan.ModelsURL
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err.Error()
	}
	if p.Kind == "codex" {
		if authorize == nil {
			return nil, "codex authorization not configured"
		}
		if err := authorize(req.Context(), req); err != nil {
			return nil, err.Error()
		}
		req.Header.Set("User-Agent", "claude-router")
	} else if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := d.Client(2 * time.Second)
	maxBytes := int64(1 << 20)
	if p.Kind == "codex" {
		client = d.CodexClient(5 * time.Second)
		maxBytes = 16 << 20
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "недоступен: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(body)) > maxBytes {
		return nil, "список моделей слишком велик"
	}
	if resp.StatusCode >= 400 || (p.Kind == "codex" && resp.StatusCode != http.StatusOK) {
		return nil, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return body, ""
}
