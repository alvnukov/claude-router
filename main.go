// localrouter dispatches explicitly configured Anthropic model/effort routes
// to Anthropic or a named pool of OpenAI-compatible and Codex models.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"
)

// respCaptureLimit bounds how much of a response the UI keeps per request.
const respCaptureLimit = 8 << 20

type config struct {
	listen        string
	upstream      *url.URL
	local         localSetup
	maxInputChars int
	failover      bool
	firstByte     time.Duration // give up on a model that has not answered by then
	balance       int           // spread requests over this many best-rated models; <2 sends everything to the first
	probeEvery    time.Duration // ping idle models this often; 0 disables

	uiListen  string
	uiHistory int
}

// Exported readers for the templates, which cannot see unexported fields.
func (c config) Local() localSetup  { return c.local }
func (c config) Failover() bool     { return c.failover }
func (c config) FirstByteSec() int  { return int(c.firstByte / time.Second) }
func (c config) Balance() int       { return c.balance }
func (c config) ProbeSec() int      { return int(c.probeEvery / time.Second) }
func (c config) MaxInputChars() int { return c.maxInputChars }
func (c config) Upstream() string   { return c.upstream.String() }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func loadConfig() config {
	c, err := loadConfigChecked()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	return c
}

func loadConfigChecked() (config, error) {
	up, err := url.Parse(env("ROUTER_UPSTREAM_URL", "https://api.anthropic.com"))
	if err != nil {
		return config{}, fmt.Errorf("bad ROUTER_UPSTREAM_URL: %w", err)
	}
	local, err := loadLocalSetupChecked(providersPath())
	if err != nil {
		return config{}, err
	}
	c := config{
		listen:   env("ROUTER_LISTEN", "127.0.0.1:8787"),
		upstream: up,
		local:    local,
	}
	c.maxInputChars = atoiOr(env("ROUTER_LOCAL_MAX_INPUT_CHARS", "0"), 0)
	c.failover = env("ROUTER_LOCAL_FAILOVER", "1") != "0"
	c.firstByte = time.Duration(atoiOr(env("ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", "45"), 45)) * time.Second
	c.balance = atoiOr(env("ROUTER_LOCAL_BALANCE", "3"), 3)
	c.probeEvery = time.Duration(atoiOr(env("ROUTER_LOCAL_PROBE_INTERVAL", "30"), 30)) * time.Second
	c.uiListen = env("ROUTER_UI_LISTEN", "127.0.0.1:8788")
	c.uiHistory = atoiOr(env("ROUTER_UI_HISTORY", "300"), 300)
	if migrated, changed := migrateLegacyPools(c.local, splitList(os.Getenv("ROUTER_CLOUD_ONLY"))); changed {
		if err := savePoolMigration(providersPath(), migrated); err != nil {
			return config{}, fmt.Errorf("pool migration: %w", err)
		}
		c.local = migrated
	}
	if migrated, changed := migrateFamilyRoutes(c.local); changed {
		if err := saveConfigurationMigration(providersPath(), migrated, ".before-families"); err != nil {
			return config{}, fmt.Errorf("family migration: %w", err)
		}
		c.local = migrated
	}
	if migrated, changed := migratePoolSettings(c); changed {
		if err := saveConfigurationMigration(providersPath(), migrated, ".before-pool-settings"); err != nil {
			return config{}, fmt.Errorf("pool settings migration: %w", err)
		}
		c.local = migrated
	}
	return c, nil
}

// Only explicit routes can serve a model. Unknown and disabled models never
// fall through to Anthropic or to another model's pool.
func (c config) routeFor(model, effort string) modelRoute { return c.local.routeFor(model, effort) }

func (c config) forModel(model, effort string) config {
	if effort == "" {
		effort = "default"
	}
	route := c.routeFor(model, effort)
	next := c
	next.local = c.local.clone()
	next.local.Models = nil
	next.local.Preferred = ""
	var targets []poolTarget
	switch route.Mode {
	case "model":
		targets = []poolTarget{{Model: route.Model, Effort: route.Effort}}
		next.failover = false
	case "pool":
		targets = c.local.ModelPools[route.Pool]
		if settings, ok := c.local.PoolSettings[route.Pool]; ok {
			next = settings.apply(next)
		}
	default:
		return next
	}
	if len(targets) == 0 {
		return next
	}
	next.local.Preferred = targets[0].Model
	for _, target := range targets {
		for _, m := range c.local.Models {
			if m.Key() == target.Model {
				m.Efforts = map[string]string{effort: target.Effort}
				next.local.Models = append(next.local.Models, m)
				break
			}
		}
	}
	return next
}

func configuredRequestRoute(cfg config, body []byte) (string, modelRoute, error) {
	var probe anthropicRequest
	if err := json.Unmarshal(body, &probe); err != nil || probe.Model == "" {
		return "", modelRoute{}, fmt.Errorf("request must contain a model")
	}
	effort := probe.OutputConfig.Effort
	if effort == "" {
		effort = "default"
	}
	route := cfg.routeFor(probe.Model, effort)
	if route.Mode == "disabled" {
		return probe.Model, route, fmt.Errorf("Для %s / %s маршрут не настроен. Назначьте Anthropic или пул моделей в настройках роутера.", probe.Model, effort)
	}
	if route.Mode == "pool" && len(cfg.local.ModelPools[route.Pool]) == 0 {
		return probe.Model, route, fmt.Errorf("Пул %s пуст. Добавьте модели в настройках роутера.", route.Pool)
	}
	return probe.Model, route, nil
}

func main() {
	loadEnvFile()
	codexAuth = newCodexAuthStore()
	cfg := loadConfig()
	cs := newConfigStore(cfg, providersPath())
	if err := cs.ensureProfiles(); err != nil {
		log.Fatalf("profile migration: %v", err)
	}
	st := newStore(cfg.uiHistory, historyPath())
	hl := newHealth(healthPath())
	cs.health = hl
	cs.watch(2 * time.Second)
	startChecker(cs, hl)
	u := newUIServer(st, cs, hl)
	u.limits = newAnthropicLimits(limitsPath(), anthropicLimitsMaxAge)
	u.startCatalogUpdates(context.Background())
	mux := newMainHandler(cfg, cs, st, hl, u)
	log.Printf("listening on %s", cfg.listen)
	log.Printf("  upstream     %s", cfg.upstream)
	log.Printf("  local        %s", cfg.local.summary())
	if cfg.uiListen != "" {
		log.Printf("  ui           http://%s (history %d)", cfg.uiListen, cfg.uiHistory)
		startUI(cfg.uiListen, u)
	}
	if err := http.ListenAndServe(cfg.listen, mux); err != nil {
		log.Fatal(err)
	}
}

func newMainHandler(cfg config, cs *configStore, st *store, hl *health, u *uiServer) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(cfg.upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
	}
	// FlushInterval -1 streams SSE through without buffering.
	proxy.FlushInterval = -1
	if u != nil {
		proxy.ModifyResponse = u.limits.observeResponse
	}

	pass := func(w http.ResponseWriter, r *http.Request, body []byte) {
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		r.Host = cfg.upstream.Host
		proxy.ServeHTTP(w, r)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/profiles/{name}/activate", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginPost(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		u.profileActivateAPI(w, r)
	})

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.get()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var probe anthropicRequest
		json.Unmarshal(body, &probe)
		_, target, routeErr := configuredRequestRoute(cfg, body)
		local := target.Mode == "pool" || target.Mode == "model"
		route := "cloud"
		if local {
			route = "local"
		}
		if routeErr != nil {
			route = "disabled"
		}
		log.Printf("req model=%q -> %s", probe.Model, route)

		rec := &record{Start: time.Now(), Path: r.URL.Path, Model: probe.Model,
			Route: route, Session: sessionOf(body), Stream: probe.Stream, Headers: pickHeaders(r.Header), ReqBody: body}
		st.add(rec)
		rw := newRecorder(w, respCaptureLimit)
		var tr *localTrace
		if local {
			tr = &localTrace{}
		}
		defer st.finish(rec.ID, rw, tr)
		if routeErr != nil {
			writeAnthropicError(rw, http.StatusBadRequest, "invalid_request_error", routeErr.Error())
			return
		}

		if !local {
			// With the client's Accept-Encoding gone, the transport negotiates
			// gzip itself and hands the proxy a decoded body, so the capture
			// sees plain bytes. Only done while the UI is on: without it the
			// upstream path stays byte-for-byte.
			if cfg.uiListen != "" {
				r.Header.Del("Accept-Encoding")
			}
			pass(rw, r, body)
			return
		}
		handleLocal(rw, r, cfg.forModel(probe.Model, probe.OutputConfig.Effort), body, tr, hl, st)
	})

	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.get()
		body, _ := io.ReadAll(r.Body)
		_, target, err := configuredRequestRoute(cfg, body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if target.Mode == "anthropic" {
			pass(w, r, body)
			return
		}

		// The local endpoint has no token-count API. Claude Code uses this only
		// for budget display, so a length-based estimate is honest enough.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"input_tokens": len(body) / 4})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		pass(w, r, nil)
	})

	return mux
}
