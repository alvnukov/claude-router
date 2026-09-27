package regression_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubReply struct {
	status int
	body   []byte
	ct     string
}

type upstreamStub struct {
	server    *httptest.Server
	mu        sync.Mutex
	calls     []observedCall
	reply     stubReply
	respond   func(http.ResponseWriter, *http.Request)
	cancelled chan struct{}
	name      string
	order     *attemptLog
}

type attemptLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *attemptLog) append(name, model string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, name+"/"+model)
}

func (l *attemptLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func newUpstreamStub(t *testing.T, body []byte) *upstreamStub {
	t.Helper()
	s := &upstreamStub{reply: stubReply{status: http.StatusOK, ct: "application/json", body: body}, cancelled: make(chan struct{}, 1)}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "synthetic request unreadable", http.StatusBadRequest)
			return
		}
		var request struct {
			Model        string `json:"model"`
			Reasoning    struct{ Effort string `json:"effort"` } `json:"reasoning"`
			OutputConfig struct{ Effort string `json:"effort"` } `json:"output_config"`
		}
		_ = json.Unmarshal(body, &request)
		s.mu.Lock()
		s.calls = append(s.calls, observedCall{Path: r.URL.Path, Model: request.Model, Effort: request.Reasoning.Effort, Body: body})
		if s.order != nil && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions") {
			s.order.append(s.name, request.Model)
		}
		if request.OutputConfig.Effort != "" {
			s.calls[len(s.calls)-1].Effort = request.OutputConfig.Effort
		}
		reply, respond := s.reply, s.respond
		s.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", reply.ct)
		w.WriteHeader(reply.status)
		_, _ = w.Write(reply.body)
	}))
	t.Cleanup(s.server.Close)
	if err := validateLoopback(s.server.URL); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *upstreamStub) setReply(status int, contentType string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = stubReply{status: status, ct: contentType, body: append([]byte(nil), body...)}
	s.respond = nil
}

func (s *upstreamStub) setResponder(fn func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = fn
}

func (s *upstreamStub) chatCalls() []observedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []observedCall
	for _, call := range s.calls {
		if call.Path == "/v1/chat/completions" || call.Path == "/chat/completions" {
			out = append(out, call)
		}
	}
	return out
}

func (s *upstreamStub) allCalls() []observedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observedCall(nil), s.calls...)
}

func validateLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 || port == 8787 || port == 8788 || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("not an approved loopback fixture URL: %q", raw)
	}
	return nil
}

func validateProviderFixture(config map[string]any) error {
	providers, ok := config["providers"].([]map[string]string)
	if !ok || len(providers) == 0 {
		return errors.New("fixture requires explicitly declared local providers")
	}
	for _, provider := range providers {
		if provider["type"] == "codex" {
			return errors.New("fixture must not create a real Codex account")
		}
		if err := validateLoopback(provider["base_url"]); err != nil {
			return fmt.Errorf("fixture provider %q: %w", provider["name"], err)
		}
	}
	return nil
}

func TestRegressionIsolationRejectsExternalFixtures(t *testing.T) {
	for _, raw := range []string{"https://example.invalid/v1", "http://example.invalid/v1", "http://127.0.0.1:8787/v1", "file:///private/home"} {
		if err := validateLoopback(raw); err == nil {
			t.Errorf("external/unsafe fixture URL %q was accepted", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:12345", "http://[::1]:12345"} {
		if err := validateLoopback(raw); err != nil {
			t.Errorf("loopback fixture URL %q was rejected: %v", raw, err)
		}
	}
	if err := validateProviderFixture(map[string]any{"providers": []map[string]string{{"name": "external", "base_url": "https://example.invalid"}}}); err == nil {
		t.Error("external provider passed fixture guard")
	}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "8787" || port == "8788" {
		t.Fatalf("unsafe test port %q: %v", address, err)
	}
	return address
}

type testFixture struct {
	profile string
	setup   func(a, b *upstreamStub) map[string]any
}

type testStand struct {
	home        string
	apiURL      string
	uiURL       string
	a, b, cloud *upstreamStub
	order       *attemptLog
	env         []string
	cmd         *exec.Cmd
	stderr      bytes.Buffer
	waited      chan struct{}
	exitErr     error
}

var binaryBuild struct {
	sync.Once
	path string
	err  error
}

// Phase A cannot attest the network and write boundary of Go, Node and child
// processes. Phase B must replace this guard only after an external preflight
// verifies an isolated container for the entire process tree. Environment
// variables, proxy settings and loopback fixtures are not such evidence.
func isolatedFullProcessTree() bool { return false }

func testBinary(t *testing.T) string {
	t.Helper()
	if !isolatedFullProcessTree() {
		t.Fatal("blocked: no independently proven process-tree network/filesystem isolation")
	}
	binaryBuild.Do(func() {
		scratch := os.Getenv("ROUTER_TEST_SCRATCH")
		if scratch == "" || !filepath.IsAbs(scratch) {
			binaryBuild.err = errors.New("blocked: ROUTER_TEST_SCRATCH must be an approved absolute scratch path")
			return
		}
		modules := os.Getenv("ROUTER_TEST_MODULE_CACHE")
		if modules == "" || !filepath.IsAbs(modules) {
			binaryBuild.err = errors.New("blocked: ROUTER_TEST_MODULE_CACHE must identify the read-only offline module cache")
			return
		}
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			binaryBuild.err = err
			return
		}
		dir, err := os.MkdirTemp(scratch, "router-regression-build-")
		if err != nil {
			binaryBuild.err = err
			return
		}
		binaryBuild.path = filepath.Join(dir, "localrouter")
		args := []string{"build"}
		if os.Getenv("ROUTER_TEST_CHILD_RACE") == "1" {
			args = append(args, "-race")
		}
		args = append(args, "-o", binaryBuild.path, ".")
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + scratch, "TMPDIR=" + scratch,
			"GOPATH=" + filepath.Join(scratch, "gopath"), "GOCACHE=" + filepath.Join(scratch, "gocache"),
			"GOMODCACHE=" + os.Getenv("ROUTER_TEST_MODULE_CACHE"),
			"GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local",
			"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			binaryBuild.err = fmt.Errorf("blocked: offline child build: %w (%s)", err, out)
		}
	})
	if binaryBuild.err != nil {
		t.Fatal(binaryBuild.err)
	}
	return binaryBuild.path
}

func defaultProfile() map[string]any {
	return map[string]any{
		"family_routes": map[string]any{"sonnet": map[string]any{
			"default": map[string]string{"mode": "pool", "pool": "a"},
			"low":     map[string]string{"mode": "pool", "pool": "b"},
			"medium":  map[string]string{"mode": "pool", "pool": "a"},
		}},
		"routes": map[string]any{"claude-sonnet-4-5-20250929": map[string]any{
			"high": map[string]string{"mode": "pool", "pool": "b"},
		}},
		"model_pools": map[string]any{
			"a": []map[string]string{{"model": "fixture-a/fixture-a-model"}},
			"b": []map[string]string{{"model": "fixture-b/fixture-b-model"}},
		},
	}
}

func setupProfiles(a, b *upstreamStub) map[string]any {
	red := defaultProfile()
	blue := defaultProfile()
	blue["family_routes"].(map[string]any)["sonnet"].(map[string]any)["default"] = map[string]string{"mode": "pool", "pool": "b"}
	return map[string]any{
		"providers": []map[string]string{
			{"name": "fixture-a", "base_url": a.server.URL + "/v1", "api_key": "fixture-local"},
			{"name": "fixture-b", "base_url": b.server.URL + "/v1", "api_key": "fixture-local"},
		},
		"models": []map[string]string{
			{"provider": "fixture-a", "model": "fixture-a-model"},
			{"provider": "fixture-b", "model": "fixture-b-model"},
		},
		"active_profile": "rr-red",
		"profiles": map[string]any{"rr-red": red, "rr-blue": blue},
		"family_routes": red["family_routes"],
		"routes": red["routes"],
		"model_pools": red["model_pools"],
	}
}

func waitReady(t *testing.T, endpoint string, stand *testStand) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 250 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-stand.waited:
			t.Fatalf("blocked: router exited before readiness: %v", stand.exitErr)
		case <-ctx.Done():
			t.Fatalf("blocked: router readiness timeout at %s: %v", endpoint, ctx.Err())
		case <-ticker.C:
		}
	}
}

func startRouter(t *testing.T, fixture testFixture) *testStand {
	t.Helper()
	if !isolatedFullProcessTree() {
		t.Fatal("blocked: full-path tests require an independently proven isolated runner")
	}
	if os.Getenv("ROUTER_TEST_SCRATCH") == "" {
		t.Fatal("blocked: isolated runner did not assign a temporary scratch directory")
	}
	home := t.TempDir()
	apiAddr, uiAddr := freeLoopbackAddress(t), freeLoopbackAddress(t)
	if apiAddr == uiAddr {
		t.Fatal("test API and UI listeners must differ")
	}
	text := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"OK-A"},"finish_reason":"stop"}]}`)
	a := newUpstreamStub(t, text)
	b := newUpstreamStub(t, bytes.Replace(text, []byte("OK-A"), []byte("OK-B"), 1))
	cloud := newUpstreamStub(t, []byte(`{"type":"message","content":[{"type":"text","text":"OK-T"}],"stop_reason":"end_turn"}`))
	order := new(attemptLog)
	a.name, a.order = "fixture-a", order
	b.name, b.order = "fixture-b", order
	for _, stub := range []*upstreamStub{a, b, cloud} {
		if err := validateLoopback(stub.server.URL); err != nil {
			t.Fatal(err)
		}
	}
	config := setupProfiles(a, b)
	if fixture.setup != nil {
		config = fixture.setup(a, b)
	}
	if fixture.profile != "" {
		config["active_profile"] = fixture.profile
	}
	if err := validateProviderFixture(config); err != nil {
		t.Fatalf("blocked: unsafe upstream fixture before child launch: %v", err)
	}
	contents, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "providers.json")
	if err := os.WriteFile(configPath, contents, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex", "xdg"} {
		if err := os.Mkdir(filepath.Join(home, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	stand := &testStand{home: home, apiURL: "http://" + apiAddr, uiURL: "http://" + uiAddr, a: a, b: b, cloud: cloud, order: order}
	for _, endpoint := range []string{stand.apiURL, stand.uiURL, cloud.server.URL} {
		if err := validateLoopback(endpoint); err != nil {
			t.Fatal(err)
		}
	}
	stand.env = []string{
		"HOME=" + home, "TMPDIR=" + os.Getenv("TMPDIR"), "XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude"), "CODEX_HOME=" + filepath.Join(home, "codex"), "ROUTER_HOME=" + home,
		"ROUTER_LISTEN=" + apiAddr, "ROUTER_PUBLIC_LISTEN=" + apiAddr, "ROUTER_UI_LISTEN=" + uiAddr,
		"ROUTER_LOCAL_PROBE_INTERVAL=0", "ROUTER_UPSTREAM_URL=" + cloud.server.URL, "ROUTER_PROVIDERS_FILE=" + configPath,
		"ROUTER_ENV_FILE=" + filepath.Join(home, "env"), "ROUTER_UI_HISTORY_FILE=" + filepath.Join(home, "history.jsonl"),
		"ROUTER_ANTHROPIC_LIMITS_FILE=" + filepath.Join(home, "limits.json"), "ROUTER_CODEX_AUTH_FILE=" + filepath.Join(home, "codex-auth.json"),
		"ROUTER_STATE_FILE=" + filepath.Join(home, "state.json"), "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1",
		"ALL_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost", "PATH=/usr/bin:/bin:/opt/homebrew/bin:/usr/local/go/bin",
	}
	t.Cleanup(func() { stand.stop(t) })
	stand.launch(t)
	return stand
}

func (s *testStand) launch(t *testing.T) {
	t.Helper()
	if s.cmd != nil {
		t.Fatal("router child already running")
	}
	s.stderr.Reset()
	s.waited = make(chan struct{})
	s.exitErr = nil
	cmd := exec.Command(testBinary(t), "serve")
	cmd.Dir = s.home
	cmd.Env = append([]string(nil), s.env...)
	cmd.Stdout = io.Discard
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("blocked: router could not start: %v", err)
	}
	s.cmd = cmd
	go func() {
		s.exitErr = cmd.Wait()
		close(s.waited)
	}()
	waitReady(t, s.apiURL+"/healthz", s)
	waitReady(t, s.uiURL+"/api/ui/state", s)
}

func (s *testStand) stop(t *testing.T) {
	t.Helper()
	if s.cmd == nil {
		return
	}
	select {
	case <-s.waited:
	default:
		_ = s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-s.waited:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.waited
			t.Error("router child did not shut down within deadline")
		}
	}
	if s.exitErr != nil || strings.Contains(s.stderr.String(), "WARNING: DATA RACE") {
		t.Errorf("router child exit/race: %v (race warning: %v)", s.exitErr, strings.Contains(s.stderr.String(), "WARNING: DATA RACE"))
	}
	s.cmd = nil
}

func (s *testStand) restart(t *testing.T) {
	t.Helper()
	s.stop(t)
	s.launch(t)
}

func (s *testStand) clientCall(t *testing.T, path string, input []byte) (int, []byte) {
	t.Helper()
	if !strings.HasPrefix(path, "/") {
		t.Fatal("relative test request path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL+path, bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fixture-local")
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
	if err != nil {
		t.Fatalf("router request was not delivered: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func (s *testStand) uiCall(t *testing.T, method, path string, body []byte, origin string) (int, []byte) {
	t.Helper()
	if !strings.HasPrefix(path, "/") {
		t.Fatal("relative UI path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, s.uiURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
	}
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func (s *testStand) activateProfile(t *testing.T, name string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"action": "profile.activate", "fields": map[string]string{"name": name}})
	if err != nil {
		t.Fatal(err)
	}
	status, body := s.uiCall(t, http.MethodPost, "/api/ui/actions", payload, s.uiURL)
	if status != http.StatusOK {
		t.Fatalf("profile activation %q: status %d (%d response bytes)", name, status, len(body))
	}
}

func requestWithEffort(model, effort, session string, stream bool) []byte {
	request := map[string]any{"model": model, "max_tokens": 64, "stream": stream, "messages": []map[string]string{{"role": "user", "content": "READY-T"}}}
	if effort != "" && effort != "default" {
		request["output_config"] = map[string]string{"effort": effort}
	}
	if session != "" {
		request["metadata"] = map[string]string{"user_id": fmt.Sprintf(`{"session_id":%s}`, strconv.Quote(session))}
	}
	body, _ := json.Marshal(request)
	return body
}
