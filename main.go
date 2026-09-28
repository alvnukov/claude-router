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
	"strings"
	"time"

	"localrouter/internal/catalogstartup"
	"localrouter/internal/cli"
	"localrouter/internal/codextesttransport"
	"localrouter/internal/history"
	"localrouter/internal/privacy"
)

// respCaptureLimit bounds how much of a response the UI keeps per request.
const respCaptureLimit = 8 << 20

// Unrecognized traffic can carry arbitrary uploads; keep only a bounded prefix.
const debugCaptureLimit = 256 << 10

type config struct {
	listen        string
	publicListen  string // where clients reach the router; a slot listens behind Caddy
	upstream      *url.URL
	local         localSetup
	maxInputChars int
	failover      bool
	firstByte     time.Duration // give up on a model that has not answered by then
	balance       int           // spread requests over this many best-rated models; <2 sends everything to the first
	probeEvery    time.Duration // ping idle models this often; 0 disables
	poolType      string        // poolFailover or poolBalance for a pool route: pool order, no rating; "" keeps the rating order
	poolName      string        // the pool a pool route resolved to; "" otherwise

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
	local, assigned, err := loadLocalSetupChecked(providersPath())
	if err != nil {
		return config{}, err
	}
	if assigned && !routerStartsStandby() {
		// One-time migration: persist the new auth_id values before the other
		// startup migrations read or rewrite the file. A standby slot writes
		// them when it is activated.
		if err := saveConfigurationMigration(providersPath(), local, ".before-codex-ids"); err != nil {
			return config{}, fmt.Errorf("codex id migration: %w", err)
		}
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
	c.publicListen = env("ROUTER_PUBLIC_LISTEN", c.listen)
	c.uiListen = env("ROUTER_UI_LISTEN", "127.0.0.1:8788")
	c.uiHistory = atoiOr(env("ROUTER_UI_HISTORY", "300"), 300)
	if routerStartsStandby() {
		return c, nil // standby migrates when it is activated, as the only writer
	}
	return migrateConfig(c, providersPath())
}

// migrateConfig brings providers.json at path to the current schema, keeping
// a backup of each step.
func migrateConfig(c config, path string) (config, error) {
	if migrated, changed := migrateLegacyPools(c.local, splitList(os.Getenv("ROUTER_CLOUD_ONLY"))); changed {
		if err := savePoolMigration(path, migrated); err != nil {
			return config{}, fmt.Errorf("pool migration: %w", err)
		}
		c.local = migrated
	}
	if migrated, changed := migrateFamilyRoutes(c.local); changed {
		if err := saveConfigurationMigration(path, migrated, ".before-families"); err != nil {
			return config{}, fmt.Errorf("family migration: %w", err)
		}
		c.local = migrated
	}
	if migrated, changed := migratePoolSettings(c); changed {
		if err := saveConfigurationMigration(path, migrated, ".before-pool-settings"); err != nil {
			return config{}, fmt.Errorf("pool settings migration: %w", err)
		}
		c.local = migrated
	}
	return c, nil
}

// Explicit version/family rules take precedence over the optional default pool.
// Disabled routes never fall through to another destination.
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
		next.poolName, next.poolType = route.Pool, poolFailover
		if settings, ok := c.local.PoolSettings[route.Pool]; ok {
			next = settings.apply(next)
			if settings.Type == poolBalance {
				next.poolType = poolBalance
			}
		}
	default:
		return next
	}
	if len(targets) == 0 {
		return next
	}
	next.local.Preferred = targets[0].Model
	for _, target := range targets {
		targetEffort := target.Effort
		if route.Mode == "pool" {
			targetEffort = target.requestEffort(effort)
		}
		for _, m := range c.local.Models {
			if m.Key() == target.Model {
				m.Efforts = map[string]string{effort: targetEffort}
				next.local.Models = append(next.local.Models, m)
				break
			}
		}
	}
	return next
}

func configuredRequestRoute(cfg config, body []byte) (string, modelRoute, error) {
	var probe anthropicRequest
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", modelRoute{}, fmt.Errorf("не удалось разобрать JSON запроса: %w", err)
	}
	if strings.TrimSpace(probe.Model) == "" {
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

func newMainHandler(cfg config, cs *configStore, st *history.Store, hl *health, u *uiServer) http.Handler {
	return newRouterHandler(cfg, cs, st, hl, u, nil)
}

// annotateMessageRecord applies the same debug policy to messages and token counts.
func annotateMessageRecord(rec *history.Record, setup localSetup, target modelRoute, routeErr, readErr error) {
	if target.Mode == "pool" && setup.defaultPoolFor(rec.Model) == target.Pool {
		rec.FallbackPool = target.Pool
		rec.UnrecognizedReason = fmt.Sprintf("Для модели нет собственного маршрута; применён пул по умолчанию %q.", target.Pool)
	}
	if routeErr != nil {
		rec.Route = "disabled"
		rec.UnrecognizedReason = routeErr.Error()
	}
	if rec.Method != http.MethodPost {
		endpoint := "API сообщений"
		if rec.Path == "/v1/messages/count_tokens" {
			endpoint = "подсчёта токенов"
		}
		rec.UnrecognizedReason = "Неожиданный HTTP-метод для " + endpoint + ": " + rec.Method
	}
	if readErr != nil {
		rec.UnrecognizedReason = "Не удалось прочитать тело запроса"
		rec.ReqTruncated = true
		rec.RequestNote = "Тело получено не полностью: ошибка чтения."
	}
	if routeErr != nil || readErr != nil {
		boundDebugRequest(rec)
	}
}

func newRouterHandler(cfg config, cs *configStore, st *history.Store, hl *health, u *uiServer, life *lifecycle) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(cfg.upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
	}
	// FlushInterval -1 streams SSE through without buffering.
	proxy.FlushInterval = -1
	if u != nil {
		proxy.ModifyResponse = u.limits.ObserveResponse
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
		var probe anthropicRequest
		_ = json.Unmarshal(body, &probe) // for the log line; routing parses the body itself
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

		rec := &history.Record{Start: time.Now(), Method: r.Method, Path: r.URL.EscapedPath(), Model: probe.Model,
			Route: route, Session: history.SessionOf(body), Stream: probe.Stream, Headers: history.PickHeaders(r.Header), ReqBody: body}
		annotateMessageRecord(rec, cfg.local, target, routeErr, err)
		st.Add(rec)
		rw := history.NewRecorder(w, respCaptureLimit)
		var tr *history.Trace
		if local {
			tr = &history.Trace{}
		}
		defer st.Finish(rec.ID, rw, tr)
		if err != nil {
			http.Error(rw, "read body", http.StatusBadRequest)
			return
		}
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
		body, readErr := io.ReadAll(r.Body)
		model, target, err := configuredRequestRoute(cfg, body)
		route := "local"
		if target.Mode == "anthropic" {
			route = "cloud"
		}
		rec := &history.Record{Start: time.Now(), Method: r.Method, Path: r.URL.EscapedPath(), Model: model, Route: route, ReqBody: body, Headers: history.PickHeaders(r.Header), Session: history.SessionOf(body)}
		annotateMessageRecord(rec, cfg.local, target, err, readErr)
		st.Add(rec)
		rw := history.NewRecorder(w, respCaptureLimit)
		defer st.Finish(rec.ID, rw, nil)
		if readErr != nil {
			http.Error(rw, "read body", http.StatusBadRequest)
			return
		}
		if err != nil {
			writeAnthropicError(rw, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if target.Mode == "anthropic" {
			pass(rw, r, body)
			return
		}

		// The local endpoint has no token-count API. Claude Code uses this only
		// for budget display, so a length-based estimate is honest enough.
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]int{"input_tokens": len(body) / 4}) // the client may be gone
	})

	// Everything else goes to Anthropic as is. One log line per request says
	// what went by: no query, headers or bodies, and a bounded, escaped path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start, path := time.Now(), r.URL.EscapedPath()
		if len(path) > 256 {
			path = path[:256] + "…"
		}
		rec := &history.Record{Start: start, Method: r.Method, Path: r.URL.EscapedPath(), Route: "passthrough", Headers: history.PickHeaders(r.Header), UnrecognizedReason: "Этот адрес не обрабатывается правилами роутера; запрос передан upstream без преобразования."}
		st.Add(rec)
		body := r.Body
		if body == nil {
			body = http.NoBody
		}
		capture := history.CaptureRequest(body, debugCaptureLimit, r.ContentLength)
		if body != http.NoBody {
			r.Body = capture
		}
		rw := history.NewRecorder(w, debugCaptureLimit)
		defer st.Finish(rec.ID, rw, nil, capture)
		pass(rw, r, nil)
		status := rw.Status()
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("pass %s %s -> %d in %s", r.Method, path, status, time.Since(start).Round(time.Millisecond))
	})
	protected := privacyTraffic(mux, cs, hl, proxy.ModifyResponse)
	if life == nil {
		return protected
	}
	api := http.NewServeMux()
	api.HandleFunc("/healthz", life.healthz)
	api.Handle("/", life.guard(protected))
	return api
}

func boundDebugRequest(rec *history.Record) {
	if len(rec.ReqBody) > debugCaptureLimit {
		rec.ReqBody = append([]byte(nil), rec.ReqBody[:debugCaptureLimit]...)
		rec.ReqTruncated = true
		if rec.RequestNote == "" {
			rec.RequestNote = "Записано начало тела: достигнут лимит диагностического захвата."
		}
	}
}

func main() {
	serve := cli.Entry{Name: "serve", Run: func(context.Context, []string, io.Writer, io.Writer) error {
		loadEnvFile()
		catalog, err := catalogstartup.ForProcess(anthropicCatalogURL, codexBaseURL)
		if err != nil {
			return fmt.Errorf("catalog startup: %w", err)
		}
		catalog.SetCodexTestTransport(codextesttransport.AuthTransport())
		defer catalog.Close()
		codexAuth = newCodexAuthStore()
		if client := catalog.SyntheticAuthClient(20 * time.Second); client != nil {
			codexAuth.client = client
		}
		cfg := loadConfig()
		for _, p := range cfg.local.Providers {
			if err := catalog.ValidateProvider(p.Name, p.Type, p.BaseURL, p.AuthID); err != nil {
				return fmt.Errorf("catalog provider %q: %w", p.Name, err)
			}
		}
		life := newLifecycle(routerStartsStandby())
		server := newRouterServer(cfg, life, statePath(), &catalog)
		if life.mode() != modeStandby {
			if err := server.cs.ensureProfiles(); err != nil {
				return fmt.Errorf("profile migration: %w", err)
			}
		}
		return server.run()
	}}
	service := cli.Commands(cli.Router{
		Label:        defaultRouterLabel,
		SlotLabels:   runServiceLabels,
		ClientURL:    routerClientURL,
		SetClaudeURL: func(url string) error { return newClaudeProxy().set(url, true) },
		ReadEnv:      readEnv,
	}, cli.SystemHost())
	table := append(append([]cli.Entry{serve}, service...),
		cli.Entry{Name: "privacy", Run: privacy.Command},
		cli.Entry{Name: "deploy", Run: func(ctx context.Context, args []string, stdout, _ io.Writer) error {
			return runDeploy(ctx, args, stdout, newDeployOps)
		}},
		cli.Entry{Name: "cutover", Run: func(ctx context.Context, args []string, stdout, _ io.Writer) error {
			return runCutover(ctx, args, os.Stdin, stdout, newCutoverOps)
		}},
		cli.Entry{Name: "service-labels", Run: func(_ context.Context, args []string, stdout, _ io.Writer) error {
			return runServiceLabels(args, stdout)
		}},
	)
	os.Exit(cli.Dispatch(context.Background(), table, os.Args[1:], os.Stdout, os.Stderr))
}
