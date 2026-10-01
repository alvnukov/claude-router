package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"localrouter/internal/catalogstartup"

	conf "localrouter/internal/config"
)

func TestParseOfficialAnthropicCatalog(t *testing.T) {
	body := []byte(`<a href="/models/claude-opus-99">old documentation slug</a><button><span>claude-opus-5-5</span><svg></svg></button><button>claude-fable-5-1</button><button>claude-opus-5-5</button><button>anthropic.claude-opus-5-5</button><button>claude-haiku-4-5@20251001</button><script>claude-sonnet-99</script>`)
	got, err := parseAnthropicCatalog(body)
	if err != nil || !reflect.DeepEqual(got, []string{"claude-fable-5-1", "claude-opus-5-5"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parseAnthropicCatalog([]byte(`<html>upstream unavailable</html>`)); err == nil {
		t.Fatal("empty/error page accepted")
	}
}

func TestCatalogRefreshMergesConcurrentEditsAndKeepsLastGood(t *testing.T) {
	t.Run("real official and Codex probes persist", func(t *testing.T) {
		requests := make(chan string, 3)
		blockCodex, codexEntered, codexCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
		fixtureHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests <- r.URL.String()
			switch r.URL.Path {
			case "/overview":
				_, _ = w.Write([]byte(`<button>claude-opus-5-5</button>`))
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
			case "/models", "/backend-api/codex/models":
				select {
				case <-blockCodex:
					close(codexEntered)
					<-r.Context().Done()
					close(codexCanceled)
					return
				default:
				}
				if r.URL.Path == "/backend-api/codex/models" && (r.TLS == nil || r.Host != "chatgpt.com") {
					t.Errorf("production Codex target was not pinned: %s %s", r.Host, r.URL)
				}
				if r.URL.Path == "/models" && r.TLS != nil {
					t.Error("synthetic Codex reached the TLS endpoint instead of loopback HTTP")
				}
				if r.Header.Get("ChatGPT-Account-Id") != "catalogsynthetic-account" {
					t.Errorf("Codex account = %q", r.Header.Get("ChatGPT-Account-Id"))
				}
				_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]}]}`))
			default:
				t.Errorf("unexpected catalog endpoint: %s", r.URL.String())
			}
		})
		fixture := httptest.NewServer(fixtureHandler)
		defer fixture.Close()
		tlsFixture := httptest.NewTLSServer(fixtureHandler)
		defer tlsFixture.Close()
		roots := x509.NewCertPool()
		roots.AddCert(tlsFixture.Certificate())
		dialer := &net.Dialer{}
		oldTransport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = oldTransport })
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"},
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == "chatgpt.com:443" {
					address = tlsFixture.Listener.Addr().String()
				} else if address != fixture.Listener.Addr().String() {
					return nil, errors.New("test catalog dial denied")
				}
				return dialer.DialContext(ctx, network, address)
			},
		}
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		authPath := filepath.Join(root, "codex-auth.json")
		t.Setenv("ROUTER_CODEX_AUTH_FILE", authPath)
		useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
		codexAuth.path = authPath
		credential := usageCredential("catalogsynthetic-account")
		credential.Tokens.AccessToken = strings.Replace(testJWT(time.Now().Add(time.Hour)), "header.", "catalogsynthetic.", 1)
		credential.Tokens.RefreshToken = "catalogsynthetic-refresh"
		if err := codexAuth.save(credential); err != nil {
			t.Fatal(err)
		}
		manifest, err := json.Marshal(map[string]any{
			"fixture_root": root, "official_url": fixture.URL + "/overview",
			"codex_models_url": fixture.URL + "/models?client_version=0.159.2",
			"provider_origins": []string{fixture.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(root, "catalog.json")
		if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", manifestPath)
		u, _ := testUI(t)
		c := u.cs.Get()
		c.Local.Providers[0].BaseURL = fixture.URL + "/v1"
		codex := provider{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}
		c.Local.Providers = append(c.Local.Providers, codex)
		u.cs = conf.NewStore(c, filepath.Join(t.TempDir(), "providers.json"))
		deps, err := catalogstartup.ForProcess(fixture.URL+"/overview", conf.CodexBaseURL)
		if err != nil {
			t.Fatal(err)
		}
		defer deps.Close()
		if deps.SyntheticAuthClient(time.Second) != nil {
			t.Run("synthetic startup does not fetch unrelated Codex usage", func(t *testing.T) {
				t.Setenv("ROUTER_PROVIDERS_FILE", "")
				server := newRouterServer(config{Local: localSetup{Providers: []provider{codex}}}, newLifecycle(false), filepath.Join(t.TempDir(), "state.json"), &deps)
				t.Run("manual Codex usage remains isolated", func(t *testing.T) {
					client := server.ui.codexUsage.client
					if client == nil {
						t.Fatal("synthetic UI usage client is not guarded")
					}
					resp, err := client.Get(codexUsageURL)
					if resp != nil {
						resp.Body.Close()
					}
					if err == nil {
						t.Fatal("synthetic manual usage reached the network")
					}
				})
				server.ui.fetchAnthropic = func(ctx context.Context) ([]string, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				usageRequests := make(chan struct{}, 1)
				server.ui.codexUsage.client = &http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
					usageRequests <- struct{}{}
					return nil, errors.New("unexpected usage request")
				})}
				server.startBackground()
				defer func() {
					server.cancel()
					wait, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					var d catalogstartup.Dependencies
					if err := d.Wait(wait, server.catalogRun.Done); err != nil {
						t.Error(err)
					}
				}()
				select {
				case <-usageRequests:
					t.Fatal("synthetic startup initiated an unrelated Codex usage request")
				case <-time.After(150 * time.Millisecond):
				}
			})
		}
		u.catalog = &deps
		u.fetchAnthropic = func(ctx context.Context) ([]string, error) { return fetchAnthropicCatalog(ctx, deps) }
		if err := u.refreshModels(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"/overview", "/v1/models", "/models?client_version=0.159.2"} {
			select {
			case got := <-requests:
				if got != want && !(want == "/models?client_version=0.159.2" && got == "/backend-api/codex"+want) {
					t.Errorf("catalog probe = %q, want %q or pinned equivalent", got, want)
				}
			case <-time.After(time.Second):
				t.Errorf("catalog probe missing: %s", want)
			}
		}
		current := u.cs.Get().Local.Catalog
		persisted, err := conf.ReadProviders(u.cs.Path())
		if err != nil || len(current.Anthropic) != 1 || len(current.Providers["p"].Models) != 2 || len(current.Providers["codex"].Models) != 1 || len(persisted.Catalog.Providers["codex"].Models) != 1 || current.CheckedAt.IsZero() {
			t.Fatalf("real catalog merge/persistence: in-memory=%+v, disk=%+v, err=%v", current, persisted.Catalog, err)
		}
		if err := os.WriteFile(u.cs.Path(), []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		close(blockCodex)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- u.refreshModels(ctx) }()
		select {
		case <-codexEntered:
		case <-time.After(time.Second):
			t.Fatal("second refresh did not reach Codex fixture")
		}
		cancel()
		select {
		case <-codexCanceled:
		case <-time.After(time.Second):
			t.Error("in-flight Codex request ignored cancellation")
		}
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Codex refresh after cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Codex refresh did not join after cancellation")
		}
		data, err := os.ReadFile(u.cs.Path())
		if err != nil || string(data) != "unchanged" {
			t.Errorf("late Codex providers.json write: %q, %v", data, err)
		}
	})

	t.Run("failed persistence retains last-good in memory", func(t *testing.T) {
		u, _ := testUI(t)
		c := u.cs.Get()
		before := c.Local.Clone()
		before.Catalog.Anthropic = []string{"claude-opus-4-1"}
		c.Local = before
		u.cs = conf.NewStore(c, filepath.Join(t.TempDir(), "blocked-directory"))
		if err := os.Mkdir(u.cs.Path(), 0700); err != nil {
			t.Fatal(err)
		}
		u.fetchAnthropic = func(context.Context) ([]string, error) { return []string{"claude-opus-5-5"}, nil }
		if err := u.refreshModels(context.Background()); err == nil {
			t.Fatal("failed providers.json write was reported as success")
		}
		if got := u.cs.Get().Local; !reflect.DeepEqual(got, before) {
			t.Fatalf("persistence failure replaced in-memory last-good catalog: %+v", got.Catalog)
		}
	})

	t.Run("cancel in-flight official fetch without a late write", func(t *testing.T) {
		entered, aborted := make(chan struct{}), make(chan struct{})
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(aborted)
		}))
		defer fixture.Close()
		u, _ := testUI(t)
		u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
		if err := os.WriteFile(u.cs.Path(), []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		deps := catalogstartup.Production(fixture.URL+"/overview", conf.CodexBaseURL)
		defer deps.Close()
		u.catalog = &deps
		u.fetchAnthropic = func(ctx context.Context) ([]string, error) { return fetchAnthropicCatalog(ctx, deps) }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- u.refreshModels(ctx) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("official fetch did not reach fixture")
		}
		cancel()
		select {
		case <-aborted:
		case <-time.After(time.Second):
			t.Error("official fetch ignored cancellation")
		}
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("official refresh after cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("official refresh did not join after cancellation")
		}
		data, err := os.ReadFile(u.cs.Path())
		if err != nil || string(data) != "unchanged" {
			t.Errorf("late official providers.json write: %q, %v", data, err)
		}
	})

	t.Run("cancel after probes before persistence boundary", func(t *testing.T) {
		u, _ := testUI(t)
		path := filepath.Join(t.TempDir(), "providers.json")
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		probed := make(chan struct{})
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(probed)
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		}))
		defer fixture.Close()
		c := u.cs.Get()
		c.Local.Providers[0].BaseURL = fixture.URL
		u.cs = conf.NewStore(c, path)
		fetching, release := make(chan struct{}), make(chan struct{})
		u.fetchAnthropic = func(context.Context) ([]string, error) {
			close(fetching)
			<-release
			return []string{"claude-opus-5-5"}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- u.refreshModels(ctx) }()
		select {
		case <-fetching:
		case <-time.After(time.Second):
			t.Fatal("catalog fetch did not start")
		}
		// An Update holds the store lock while its function runs; the refused
		// edit changes nothing.
		held, unlock, unlocked := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(unlocked)
			_ = u.cs.Update(func(*localSetup) error {
				close(held)
				<-unlock
				return errors.New("lock held by test")
			})
		}()
		<-held
		locked := true
		defer func() {
			if locked {
				close(unlock)
				<-unlocked
			}
		}()
		close(release)
		select {
		case <-probed:
		case <-time.After(time.Second):
			t.Fatal("provider probe did not reach fixture")
		}
		// probeMu unlocks only after the response was parsed. Holding cs.mu
		// keeps the persistence admission check beyond this cancellation.
		u.probeMu.Lock()
		u.probeMu.Unlock()
		cancel()
		close(unlock)
		<-unlocked
		locked = false
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("refresh after cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled refresh did not return")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Errorf("canceled refresh wrote providers.json: %q, %v", data, err)
		}
	})

	t.Run("cancel in-flight provider without a late write", func(t *testing.T) {
		u, _ := testUI(t)
		path := filepath.Join(t.TempDir(), "providers.json")
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		entered, aborted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-r.Context().Done():
				close(aborted)
			case <-release:
				_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			}
		}))
		defer fixture.Close()
		c := u.cs.Get()
		c.Local.Providers[0].BaseURL = fixture.URL
		u.cs = conf.NewStore(c, path)
		u.fetchAnthropic = func(context.Context) ([]string, error) { return []string{"claude-opus-5-5"}, nil }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- u.refreshModels(ctx) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("provider was not probed")
		}
		cancel()
		canceledHTTP := false
		select {
		case <-aborted:
			canceledHTTP = true
		case <-time.After(time.Second):
		}
		close(release)
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("refresh after cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("refresh did not join after cancellation")
		}
		if !canceledHTTP {
			t.Error("in-flight provider HTTP did not receive cancellation")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Errorf("late providers.json write: %q, %v", data, err)
		}
	})

	u, _ := testUI(t)
	path := filepath.Join(t.TempDir(), "providers.json")
	u.cs = conf.NewStore(u.cs.Get(), path)
	entered, release := make(chan struct{}), make(chan struct{})
	u.fetchAnthropic = func(context.Context) ([]string, error) {
		close(entered)
		<-release
		return []string{"claude-opus-5-5", "claude-fable-5-1"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- u.refreshModels(context.Background()) }()
	<-entered
	l := u.cs.Get().Local.Clone()
	l.FamilyRoutes = map[string]map[string]modelRoute{"opus": {"high": {Mode: "anthropic"}}}
	if err := u.cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current := u.cs.Get().Local
	if current.RouteFor("claude-opus-5-5", "high").Mode != "anthropic" || len(current.Catalog.Anthropic) != 2 || len(current.Catalog.Providers["p"].Models) != 2 {
		t.Fatal("refresh overwrote routes or lost a catalog")
	}
	updated := current.Catalog.AnthropicUpdated
	u.fetchAnthropic = func(context.Context) ([]string, error) { return nil, errors.New("temporary outage") }
	if err := u.refreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := u.cs.Get().Local
	if !reflect.DeepEqual(after.Catalog.Anthropic, current.Catalog.Anthropic) || !after.Catalog.AnthropicUpdated.Equal(updated) || len(after.Catalog.Notes) != 1 {
		t.Fatal("failed refresh destroyed cache or hid failure")
	}
	reloaded, err := conf.ReadProviders(path)
	if err != nil || !reflect.DeepEqual(reloaded.Catalog.Anthropic, after.Catalog.Anthropic) || reloaded.RouteFor("claude-opus-6", "high").Mode != "anthropic" {
		t.Fatalf("catalog persistence: %v", err)
	}
}

func TestCodexEffortControlsUseSelectedModelCatalog(t *testing.T) {
	u, h := testUI(t)
	p := provider{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}
	l := localSetup{Providers: []provider{p}, Models: []localModel{{Provider: "codex", Model: "gpt-6-sol"}, {Provider: "codex", Model: "gpt-6-luna"}}, ModelPools: map[string][]poolTarget{"work": {}}}
	c := u.cs.Get()
	c.Local = l
	u.cs = conf.NewStore(c, filepath.Join(t.TempDir(), "providers.json"))
	u.probe = map[string]probeResult{"codex": {At: time.Now(), OK: true, Base: conf.CodexBaseURL, Models: []string{"gpt-6-sol", "gpt-6-luna"}, Info: []probeModel{{ID: "gpt-6-sol", Efforts: []string{"low", "high", "ultra"}}, {ID: "gpt-6-luna", Efforts: []string{"low", "high"}}}}}
	for _, tc := range []struct {
		key   string
		ultra bool
	}{{"codex/gpt-6-sol", true}, {"codex/gpt-6-luna", false}} {
		body := get(t, h, "GET", "/settings/pool-add?"+url.Values{"name": {"work"}, "key": {tc.key}}.Encode(), nil).Body.String()
		if strings.Contains(body, `value="ultra"`) != tc.ultra || strings.Contains(body, `value="none"`) || strings.Contains(body, `value="minimal"`) {
			t.Fatalf("invented effort options: %s", body)
		}
	}
	get(t, h, "POST", "/settings/pools", url.Values{"name": {"work"}, "op": {"add"}, "key": {"codex/gpt-6-luna"}, "effort": {"ultra"}})
	if len(u.cs.Get().Local.ModelPools["work"]) != 0 {
		t.Fatal("unsupported effort accepted")
	}
	get(t, h, "POST", "/settings/pools", url.Values{"name": {"work"}, "op": {"add"}, "key": {"codex/gpt-6-luna"}, "effort": {"high"}})
	if len(u.cs.Get().Local.ModelPools["work"]) != 1 {
		t.Fatal("supported effort rejected")
	}
	if opts := modelEffortOptions(l, "codex/gpt-6-sol", nil); len(opts) != 0 {
		t.Fatal("missing catalog fell back to invented levels")
	}
	if opts := modelEffortOptions(l, "codex/gpt-6-sol", map[string]probeModel{"codex/gpt-6-sol": {}}); slices.Contains(opts, "high") {
		t.Fatal("empty advertised levels ignored")
	}
}

func TestGlobalRefreshButtonUsesSameUpdater(t *testing.T) {
	u, h := testUI(t)
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	u.fetchAnthropic = func(context.Context) ([]string, error) { return []string{"claude-opus-5-5"}, nil }
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://localhost/settings/refresh-models", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Проверка моделей завершена") || len(u.cs.Get().Local.Catalog.Anthropic) != 1 {
		t.Fatalf("refresh: %d", w.Code)
	}
	t.Run("cancel settings provider probe", func(t *testing.T) {
		entered, aborted := make(chan struct{}), make(chan struct{})
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(aborted)
		}))
		defer fixture.Close()
		u, handler := testUI(t)
		c := u.cs.Get()
		c.Local.Providers[0].BaseURL = fixture.URL
		u.cs = conf.NewStore(c, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "http://localhost/settings/probe", strings.NewReader("provider=p")).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), req)
			close(done)
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("settings provider probe did not reach fixture")
		}
		cancel()
		select {
		case <-aborted:
		case <-time.After(time.Second):
			t.Error("settings probe HTTP did not receive request cancellation")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("settings probe did not return after cancellation")
		}
	})
	t.Run("cancel profile response probe", func(t *testing.T) {
		entered, aborted := make(chan struct{}), make(chan struct{})
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(aborted)
		}))
		defer fixture.Close()
		u, handler := testUI(t)
		c := u.cs.Get()
		c.Local.Providers[0].BaseURL = fixture.URL
		u.cs = conf.NewStore(c, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "http://localhost/settings/profiles/activate", strings.NewReader("name=missing")).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), req)
			close(done)
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("profile response did not probe provider")
		}
		cancel()
		select {
		case <-aborted:
		case <-time.After(time.Second):
			t.Error("profile response probe ignored cancellation")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("profile response did not return after cancellation")
		}
	})
}
