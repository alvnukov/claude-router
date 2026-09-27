package regression_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completedHistoryOnDisk(t *testing.T, stand *testStand, list map[string]any) bool {
	t.Helper()
	entries, ok := list["items"].([]any)
	if !ok {
		return false
	}
	if len(entries) == 0 {
		return true
	}
	ids := make(map[string]bool, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok || item["pending"] != false {
			return false
		}
		id, ok := item["id"].(string)
		if !ok || !strings.HasPrefix(id, "req_") {
			return false
		}
		ids[id] = false
	}
	data, err := os.ReadFile(filepath.Join(stand.home, "history.jsonl"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return false
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		value, err := parseJSON(line)
		if err != nil {
			return false // the writer may still be appending the last line
		}
		record, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if id, ok := record["ID"].(string); ok {
			if _, requested := ids[id]; requested {
				ids[id] = true
			}
		}
	}
	for _, present := range ids {
		if !present {
			return false
		}
	}
	return true
}

func requestList(t *testing.T, stand *testStand, query url.Values, expectedTotal int) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, body := stand.uiCall(t, http.MethodGet, "/api/ui/requests?"+query.Encode(), nil, "")
		if status != http.StatusOK {
			t.Fatalf("history list returned %d", status)
		}
		got := observedMessage(t, body)
		if got["total"] == json.Number(fmt.Sprint(expectedTotal)) && completedHistoryOnDisk(t, stand, got) {
			return got
		}
		select {
		case <-ctx.Done():
			t.Fatalf("history total/completion/persistence did not reach %d (observed total %v)", expectedTotal, got["total"])
		case <-ticker.C:
		}
	}
}

func requestIDs(t *testing.T, list map[string]any) []string {
	t.Helper()
	entries, ok := list["items"].([]any)
	if !ok {
		t.Fatalf("history list has no items array: %v", list)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatal("history item is not an object")
		}
		id, ok := item["id"].(string)
		if !ok || !strings.HasPrefix(id, "req_") {
			t.Fatal("history item lacks a generated request ID")
		}
		ids = append(ids, id)
	}
	return ids
}

func sameIDs(observed, expected []string) bool {
	if len(observed) != len(expected) {
		return false
	}
	counts := make(map[string]int, len(expected))
	for _, id := range expected {
		counts[id]++
	}
	for _, id := range observed {
		counts[id]--
		if counts[id] < 0 {
			return false
		}
	}
	return true
}

func TestRegressionHistoryOracleRejectsExtraSimilarModel(t *testing.T) {
	expected := []string{"req_one", "req_two"}
	if !sameIDs([]string{"req_two", "req_one"}, expected) {
		t.Fatal("positive exact-set oracle failed")
	}
	for _, broken := range [][]string{{"req_one"}, {"req_one", "req_two", "req_extra"}, {"req_one", "req_one"}} {
		if sameIDs(broken, expected) {
			t.Fatal("altered observed history ID set passed immutable oracle")
		}
	}
}

func TestRegressionHistoryDetailAndExactFilter(t *testing.T) {
	stand := startRouter(t, testFixture{setup: func(a, b *upstreamStub) map[string]any {
		config := setupProfiles(a, b)
		config["default_pool"] = "a"
		config["profiles"].(map[string]any)["rr-red"].(map[string]any)["default_pool"] = "a"
		return config
	}})
	var scenarios []struct {
		Model   string `json:"model"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(wireFixtureFrom(t, "history", "similar-models.json"), &scenarios); err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("RR-HIS-02: manual fixture has %d cases, expected 2", len(scenarios))
	}
	for _, row := range scenarios {
		status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(row.Model, "default", row.Session, false))
		if status != http.StatusOK {
			t.Fatalf("RR-HIS-01: %s returned status %d", row.Model, status)
		}
		if err := checkRoutedMessage(body, row.Model, "OK-A"); err != nil {
			t.Error(err)
		}
	}
	all := requestList(t, stand, nil, 2)
	ids := requestIDs(t, all)
	if len(ids) != 2 {
		t.Fatalf("RR-HIS-02: got %d requests, want 2", len(ids))
	}
	one := requestIDs(t, requestList(t, stand, url.Values{"session": {"h-one"}}, 1))
	two := requestIDs(t, requestList(t, stand, url.Values{"session": {"h-two"}}, 1))
	if !sameIDs(ids, []string{one[0], two[0]}) || one[0] == two[0] {
		t.Error("session filter did not preserve exact distinct request IDs")
	}
	for _, row := range []struct {
		model string
		want  []string
	}{
		{"claude-opus-5", []string{one[0]}}, // similar 5-5 must not leak into the exact model set
		{"claude-opus-5-5", []string{two[0]}},
	} {
		status, raw := stand.uiCall(t, http.MethodGet, "/api/ui/requests?"+url.Values{"model": {row.model}}.Encode(), nil, "")
		if status != http.StatusOK {
			t.Fatalf("RR-HIS-02: model filter %s returned %d", row.model, status)
		}
		filter := observedMessage(t, raw)
		if got := requestIDs(t, filter); !sameIDs(got, row.want) || filter["total"] != json.Number(fmt.Sprint(len(row.want))) {
			t.Errorf("RR-HIS-02: filter %s got IDs %v total %v; expected exact set %v", row.model, got, filter["total"], row.want)
		}
	}
	path := filepath.Join(stand.home, "history.jsonl")
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !bytes.Contains(persisted, []byte(id)) {
			t.Errorf("RR-HIS-01: request %s not persisted to own history file", id)
		}
	}
	stand.restart(t)
	if got := requestIDs(t, requestList(t, stand, nil, 2)); !sameIDs(got, ids) {
		t.Errorf("RR-HIS-01: new process lost request IDs: %v", got)
	}
	if status, _ := action(t, stand, "requests.clear", map[string]string{}); status != http.StatusOK {
		t.Fatalf("RR-HIS-01: own-home clear returned %d", status)
	}
	stand.restart(t)
	if got := requestIDs(t, requestList(t, stand, nil, 0)); len(got) != 0 {
		t.Errorf("RR-HIS-01: cleared history survived restart: %v", got)
	}
}

func TestRegressionHistoryToolOnlyDetail(t *testing.T) {
	stand := startRouter(t, testFixture{})
	stand.a.setReply(http.StatusOK, "application/json", wireFixture(t, "openai-tool.json"))
	status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "tool-history", false))
	if status != http.StatusOK {
		t.Fatalf("RR-HIS-02: tool response status %d", status)
	}
	wantTool := observedMessage(t, wireFixtureFrom(t, "history", "tool-only.json"))
	blocks, ok := wantTool["content"].([]any)
	if !ok || wantTool["stop_reason"] != "tool_use" {
		t.Fatal("RR-HIS-02: invalid fixed tool-only history fixture")
	}
	assertMessageFields(t, body, blocks, "tool_use")
	id := requestIDs(t, requestList(t, stand, url.Values{"session": {"tool-history"}}, 1))[0]
	status, raw := stand.uiCall(t, http.MethodGet, "/api/ui/requests/"+url.PathEscape(id), nil, "")
	if status != http.StatusOK {
		t.Fatalf("RR-HIS-02: detail returned %d", status)
	}
	detail := observedMessage(t, raw)
	response, ok := detail["response"].(string)
	if !ok || !strings.Contains(response, "fixture_lookup") || !strings.Contains(response, "tool_fixture_1") {
		t.Error("RR-HIS-02: tool-only detail hides tool ID/name; no text-only proxy proves tool delivery")
	}
	persisted, err := os.ReadFile(filepath.Join(stand.home, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(persisted, []byte("tool_fixture_1")) || !bytes.Contains(persisted, []byte("fixture_lookup")) {
		t.Error("RR-HIS-02: tool block missing from persisted record")
	}
}

func TestRegressionHistoryLiteralJSONText(t *testing.T) {
	stand := startRouter(t, testFixture{})
	stand.a.setReply(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"{\"x\":1}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "literal-history", false))
	if status != http.StatusOK {
		t.Fatalf("RR-HIS-02: literal JSON response status %d", status)
	}
	fixed := observedMessage(t, wireFixtureFrom(t, "history", "literal-json.json"))
	blocks, ok := fixed["content"].([]any)
	if !ok || fixed["stop_reason"] != "end_turn" {
		t.Fatal("RR-HIS-02: invalid fixed literal-JSON history fixture")
	}
	assertMessageFields(t, body, blocks, "end_turn")
	id := requestIDs(t, requestList(t, stand, url.Values{"session": {"literal-history"}}, 1))[0]
	status, raw := stand.uiCall(t, http.MethodGet, "/api/ui/requests/"+url.PathEscape(id), nil, "")
	if status != http.StatusOK {
		t.Fatalf("RR-HIS-02: literal JSON detail status %d", status)
	}
	if response := observedMessage(t, raw)["response"]; response != `{"x":1}` {
		t.Errorf("RR-HIS-02: literal JSON text was reparsed or lost: %v", response)
	}
	if err := compareCalls(stand.a.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model"}}); err != nil {
		t.Error(err)
	}
	if len(stand.b.allCalls()) != 0 || len(stand.cloud.allCalls()) != 0 {
		t.Error("RR-HIS-02: forbidden upstream called for literal JSON history")
	}
}

func TestRegressionUIDetailCapsLongResponse(t *testing.T) {
	stand := startRouter(t, testFixture{})
	longText := strings.Repeat("R", (256<<10)+7)
	upstream, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]string{"role": "assistant", "content": longText},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stand.a.setReply(http.StatusOK, "application/json", upstream)
	status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "cap-fixture", false))
	if status != http.StatusOK || checkRoutedMessage(body, wireModel, longText) != nil {
		t.Fatalf("RR-UI-02: long synthetic response status %d or body mismatch", status)
	}
	id := requestIDs(t, requestList(t, stand, url.Values{"session": {"cap-fixture"}}, 1))[0]
	status, raw := stand.uiCall(t, http.MethodGet, "/api/ui/requests/"+url.PathEscape(id), nil, "")
	if status != http.StatusOK {
		t.Fatalf("RR-UI-02: long detail status %d", status)
	}
	detail := observedMessage(t, raw)
	if detail["truncated"] != true || detail["captureTruncated"] != false || detail["response"] != strings.Repeat("R", 256<<10)+"\n…" {
		t.Errorf("RR-UI-02: long response cap/marker incorrect (truncated=%v, capture=%v)", detail["truncated"], detail["captureTruncated"])
	}
	if err := compareCalls(stand.a.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model"}}); err != nil {
		t.Error(err)
	}
	if len(stand.b.allCalls()) != 0 || len(stand.cloud.allCalls()) != 0 {
		t.Error("RR-UI-02: long detail touched an unrelated upstream")
	}
}

func TestRegressionUIHostOriginEventsAndCap(t *testing.T) {
	stand := startRouter(t, testFixture{})
	configFile := filepath.Join(stand.home, "providers.json")
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"action":"profile.activate","fields":{"name":"rr-blue"}}`)
	for _, row := range []struct{ host, origin string }{
		{"other.example", ""}, {"", "http://other.example"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, stand.uiURL+"/api/ui/actions", bytes.NewReader(payload))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", row.origin)
		if row.host != "" {
			request.Host = row.host
		}
		response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		_ = response.Body.Close()
		cancel()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("RR-UI-02: foreign host/origin returned %d, want 403", response.StatusCode)
		}
	}
	after, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("RR-UI-02: rejected cross-site mutation changed config")
	}
	stateStatus, state := stand.uiCall(t, http.MethodGet, "/api/ui/state", nil, "")
	if stateStatus != http.StatusOK || observedMessage(t, state)["activeProfile"] != "rr-red" {
		t.Error("RR-UI-02: foreign action changed active profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, stand.uiURL+"/api/ui/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
	if err != nil {
		t.Fatalf("blocked: UI event channel not delivered: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("RR-UI-02: events status %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: refresh\n" {
		t.Errorf("RR-UI-02: first SSE event %q (%v)", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "data: {}\n" {
		t.Errorf("RR-UI-02: refresh payload %q (%v)", line, err)
	}
}

func TestRegressionLimitsAccountFreshness(t *testing.T) {
	// A real age-based limit test needs a controlled clock and two independent
	// synthetic account windows. Current process paths lack a safe clock seam;
	// treating an uninstrumented wall clock or a fake DTO as freshness evidence
	// would silently promote stale B's window to fresh A.
	t.Run("RR-LIM-01/account-window", func(t *testing.T) {
		t.Skip("blocked RR-LIM-01: controlled account-window clock and safe two-account fixture not established; component suites and UI usage check remain separate evidence")
	})
}

func TestRegressionHealthAndCancel(t *testing.T) {
	// Stream cancellation is covered by TestRegressionCancelNoRetry. Cooldown
	// expiry and recovery require a controlled clock; do not replace that
	// contractual transition with a sleep or just a stub status change.
	t.Run("RR-HEA-01/recovery", func(t *testing.T) {
		t.Skip("blocked RR-HEA-01 recovery: no controlled cooldown clock seam; cancellation full path is a separate test")
	})
}

func TestRegressionUIOracleRejectsForeignMutation(t *testing.T) {
	fixed := []byte(`{"activeProfile":"rr-red"}`)
	if err := compareJSON(fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := compareJSON([]byte(`{"activeProfile":"rr-blue"}`), fixed); err == nil {
		t.Error("foreign mutation was accepted by immutable UI-state oracle")
	}
	if sameIDs([]string{"req_1", "req_extra"}, []string{"req_1"}) {
		t.Error("corrupted observed history with extra request passed exact-set oracle")
	}
}
