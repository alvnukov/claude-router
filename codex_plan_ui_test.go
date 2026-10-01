package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestChatGPTPlanStateAndQuotaIsolation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		useTestCodexHome(t, "https://auth.openai.com", http.DefaultClient)
		if err := codexAuth.save(planFixture(t, "https://auth.openai.com", enabled, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
		u, _ := codexUI(t)
		state := (uiBackend{u}).State(context.Background())
		raw, _ := json.Marshal(state)
		var dto map[string]any
		json.Unmarshal(raw, &dto)
		if dto["version"] == nil || dto["version"] == "" {
			t.Error("router build version absent")
		}
		for _, entry := range dto["connections"].([]any) {
			v := entry.(map[string]any)
			if v["name"] == "codex" {
				if v["authMode"] != "chatgpt-plan" || v["subscriptionEnabled"] != enabled || v["usageURL"] != "https://chatgpt.com/#settings/Usage" {
					t.Error("safe plan status absent")
				}
			}
		}
		hits := 0
		cache := &codexUsageCache{}
		cache.client = &http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) { hits++; return usageResponse(200, `{}`), nil })}
		view := cache.get(context.Background(), codexAuth, true)
		if hits != 0 || !view.Connected || len(view.Limits) != 0 || view.Error != "" {
			t.Errorf("plan grant used private quota endpoint: hits=%d %+v", hits, view)
		}
	}
}
