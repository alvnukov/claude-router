// localrouter sits between Claude Code and api.anthropic.com.
//
// Requests naming the local model are translated to an OpenAI-compatible
// endpoint. Everything else is forwarded to Anthropic byte for byte: the
// upstream path never parses a body and never sees a rewritten header, so
// normal cloud traffic behaves exactly as it would without the proxy.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// respCaptureLimit bounds how much of a response the UI keeps per request.
const respCaptureLimit = 8 << 20

type config struct {
	listen        string
	upstream      *url.URL
	local         localSetup
	cloudOnly     []string
	maxInputChars int
	// extraLocal holds model ids that used to be the local model; see configStore.
	extraLocal []string
	failover   bool
	firstByte  time.Duration // give up on a model that has not answered by then
	balance    int           // spread requests over this many best-rated models; <2 sends everything to the first
	probeEvery time.Duration // ping idle models this often; 0 disables

	uiListen  string
	uiHistory int
}

// Exported readers for the templates, which cannot see unexported fields.
func (c config) Local() localSetup   { return c.local }
func (c config) Failover() bool      { return c.failover }
func (c config) FirstByteSec() int   { return int(c.firstByte / time.Second) }
func (c config) Balance() int        { return c.balance }
func (c config) ProbeSec() int       { return int(c.probeEvery / time.Second) }
func (c config) CloudOnly() []string { return c.cloudOnly }
func (c config) MaxInputChars() int  { return c.maxInputChars }
func (c config) Upstream() string    { return c.upstream.String() }

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
	for _, s := range strings.Split(os.Getenv("ROUTER_CLOUD_ONLY"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.cloudOnly = append(c.cloudOnly, s)
		}
	}
	return c
}

// isLocal decides whether a model id belongs to the local endpoint. Any
// "local-" prefixed id is accepted as an extra alias for the same single
// ROUTER_LOCAL_MODEL, so an alias can be renamed without touching the config.
//
// ROUTER_CLOUD_ONLY inverts the default: with it set, the listed substrings are
// the only models that still reach Anthropic and everything else -- a subagent
// asked for on sonnet, the small model behind a page fetch -- is answered
// locally. That is the point of the switch: a model id chosen somewhere else in
// the tool, by a subagent or by a background chore, can no longer put the
// prompt on the network by accident. An empty model is never claimed: an
// unrecognised body is forwarded rather than interpreted.
func (c config) isLocal(model string) bool {
	if strings.HasPrefix(model, "local-") {
		return true
	}
	for _, m := range c.local.Models {
		if model == m.Model {
			return true
		}
	}
	for _, m := range c.extraLocal {
		if model == m {
			return true
		}
	}
	if len(c.cloudOnly) == 0 || model == "" {
		return false
	}
	for _, s := range c.cloudOnly {
		if strings.Contains(model, s) {
			return false
		}
	}
	return true
}

func main() {
	loadEnvFile()
	cfg := loadConfig()
	cs := newConfigStore(cfg, providersPath())
	cs.watch(2 * time.Second)
	st := newStore(cfg.uiHistory, historyPath())
	hl := newHealth(healthPath())
	startChecker(cs, hl)

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
		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		// A body we cannot parse is not ours to interpret: forward it.
		local := json.Unmarshal(body, &probe) == nil && cfg.isLocal(probe.Model)
		route := "cloud"
		if local {
			route = "local"
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
		handleLocal(rw, r, cfg, body, tr, hl)
	})

	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.get()
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &probe) != nil || !cfg.isLocal(probe.Model) {
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

	log.Printf("listening on %s", cfg.listen)
	log.Printf("  upstream     %s", cfg.upstream)
	log.Printf("  local        %s", cfg.local.summary())
	if len(cfg.cloudOnly) > 0 {
		log.Printf("  cloud only   %s (everything else goes local)", strings.Join(cfg.cloudOnly, ", "))
	}
	if cfg.uiListen != "" {
		log.Printf("  ui           http://%s (history %d)", cfg.uiListen, cfg.uiHistory)
		startUI(cfg.uiListen, st, cs, hl)
	}
	if err := http.ListenAndServe(cfg.listen, mux); err != nil {
		log.Fatal(err)
	}
}
