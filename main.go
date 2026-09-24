// localrouter dispatches explicitly configured Anthropic model/effort routes
// to Anthropic or a named pool of OpenAI-compatible and Codex models.
package main

import (
	"bytes"
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
	up, err := url.Parse(env("ROUTER_UPSTREAM_URL", "https://api.anthropic.com"))
	if err != nil {
		log.Fatalf("bad ROUTER_UPSTREAM_URL: %v", err)
	}
	c := config{
		listen:   env("ROUTER_LISTEN", "127.0.0.1:8787"),
		upstream: up,
		local:    loadLocalSetup(providersPath()),
	}
	c.maxInputChars = atoiOr(env("ROUTER_LOCAL_MAX_INPUT_CHARS", "0"), 0)
	c.failover = env("ROUTER_LOCAL_FAILOVER", "1") != "0"
	c.firstByte = time.Duration(atoiOr(env("ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", "45"), 45)) * time.Second
	c.balance = atoiOr(env("ROUTER_LOCAL_BALANCE", "3"), 3)
	c.probeEvery = time.Duration(atoiOr(env("ROUTER_LOCAL_PROBE_INTERVAL", "30"), 30)) * time.Second
	c.uiListen = env("ROUTER_UI_LISTEN", "127.0.0.1:8788")
	c.uiHistory = atoiOr(env("ROUTER_UI_HISTORY", "300"), 300)
	if os.Getenv("ROUTER_STANDBY") == "1" {
		return c // standby reads config but never runs a migration that writes it
	}
	if migrated, changed := migrateLegacyPools(c.local, splitList(os.Getenv("ROUTER_CLOUD_ONLY"))); changed {
		if err := savePoolMigration(providersPath(), migrated); err != nil {
			log.Fatalf("pool migration: %v", err)
		}
		c.local = migrated
	}
	if migrated, changed := migrateFamilyRoutes(c.local); changed {
		if err := saveConfigurationMigration(providersPath(), migrated, ".before-families"); err != nil {
			log.Fatalf("family migration: %v", err)
		}
		c.local = migrated
	}
	if migrated, changed := migratePoolSettings(c); changed {
		if err := saveConfigurationMigration(providersPath(), migrated, ".before-pool-settings"); err != nil {
			log.Fatalf("pool settings migration: %v", err)
		}
		c.local = migrated
	}
	return c
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

func newRouterHandler(cfg config, cs *configStore, st *store, hl *health, life *lifecycle) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(cfg.upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
	}
	// FlushInterval -1 streams SSE through without buffering.
	proxy.FlushInterval = -1

	pass := func(w http.ResponseWriter, r *http.Request, body []byte) {
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		r.Host = cfg.upstream.Host
		proxy.ServeHTTP(w, r)
	}

	mux := http.NewServeMux()

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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		pass(w, r, nil)
	})
	admin := newRuntimeAdmin(life, hl, statePath())
	api := http.NewServeMux()
	api.HandleFunc("/healthz", life.healthz)
	api.Handle("/admin/", admin)
	api.Handle("/", life.guard(mux))
	return api
}

func main() {
	loadEnvFile()
	codexAuth = newCodexAuthStore()
	cfg := loadConfig()
	life := newLifecycle(os.Getenv("ROUTER_STANDBY") == "1")
	server := newRouterServer(cfg, life, statePath())
	if err := server.run(); err != nil {
		log.Fatal(err)
	}
}
