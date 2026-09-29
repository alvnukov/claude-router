package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"localrouter/internal/catalogstartup"
	"localrouter/internal/codextesttransport"
	conf "localrouter/internal/config"
	"localrouter/internal/history"
	"localrouter/internal/limits"
	"localrouter/internal/privacy"
	webui "localrouter/internal/ui"
	"localrouter/internal/uisession"
)

//go:embed ui/*
var uiFS embed.FS

// uiServer is the inspection and control panel. It binds to its own address
// (loopback by default) so nothing about it touches the API path, and it has
// no auth: everything it shows is the traffic of the user running it.
type uiServer struct {
	privacyOnce       sync.Once
	privacyLab        *privacy.Lab
	codexUsage        codexUsageCache // template: per-connection caches copy its client
	usageMu           sync.Mutex
	codexUsages       map[string]*codexUsageCache // by name + "\x00" + auth id
	limits            *limits.Store
	claudeProxy       *claudeProxy
	catalogMu         sync.Mutex
	catalog           *catalogstartup.Dependencies
	fetchAnthropic    func(context.Context) ([]string, error)
	st                *history.Store
	connectionMetrics connectionUsageCache
	sessionNames      uisession.Names
	sessionBodies     uisession.Bodies
	cs                *configStore
	hl                *health
	life              *lifecycle
	tpl               *template.Template
	started           time.Time

	probeMu     sync.Mutex
	probe       map[string]probeResult // by provider name
	oauthMu     sync.Mutex
	oauthFlow   *codexBrowserFlow
	oauthTarget provider // the connection the pending login is for
	oauthStatus string
	oauthError  string
}

type probeResult struct {
	At     time.Time
	OK     bool
	Msg    string
	Models []string // ids, sorted
	Info   []probeModel
	Facets []facet // derived from Info, with no value picked
	Base   string
	Key    string // api key the probe used; a change invalidates the cache
	AuthID string // Codex connection the probe used; a recreated one re-probes
}

// uiTemplates parses the embedded pages. Kept apart from startUI so a test
// can catch a broken template before launchd does.
func uiTemplates() (*template.Template, error) {
	funcs := template.FuncMap{
		"kb":     fmtChars,
		"fmtnum": fmtNum,
		"dur":    fmtDur,
		"ago":    fmtAgo,
		"base":   filepath.Base,
		"pct":    func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		// percent drops a zero fraction: 77, 4.5.
		"percent": func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) },
		"score":   func(f float64) string { return strconv.FormatFloat(f*100, 'f', 0, 64) + "%" },
		"ms": func(f float64) string {
			if f == 0 {
				return "—"
			}
			return fmtDur(time.Duration(f * float64(time.Millisecond)))
		},
		"tsec":  func(t time.Time) string { return t.Format("15:04:05") },
		"tfull": func(t time.Time) string { return t.Format("2006-01-02 15:04:05.000") },
		"join":  strings.Join,
		"add":   func(a, b int) int { return a + b },
		"lower": strings.ToLower,
		"dict2": func(c *ctxView, h map[string]string, q string) ctxArgs { return ctxArgs{Ctx: c, Headers: h, Q: q} },
		"short": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
	}
	return template.New("").Funcs(funcs).ParseFS(uiFS, "ui/*.html")
}

func newUIServer(st *history.Store, cs *configStore, hl *health, catalog ...*catalogstartup.Dependencies) *uiServer {
	u := &uiServer{claudeProxy: newClaudeProxy(), fetchAnthropic: func(ctx context.Context) ([]string, error) { return fetchAnthropicCatalog(ctx) }, st: st, cs: cs, tpl: template.Must(uiTemplates()), started: time.Now(), hl: hl, limits: limits.New("", limits.MaxAge)}
	if len(catalog) > 0 && catalog[0] != nil {
		u.catalog = catalog[0]
		u.fetchAnthropic = func(ctx context.Context) ([]string, error) { return fetchAnthropicCatalog(ctx, *catalog[0]) }
		u.codexUsage.client = catalog[0].SyntheticAuthClient(10 * time.Second)
	}
	return u
}

func (u *uiServer) handler() http.Handler {
	static, _ := fs.Sub(uiFS, "ui")
	mux := http.NewServeMux()
	webui.Mount(mux, uiBackend{u})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /status", u.status)
	mux.HandleFunc("POST /requests/clear", u.clear)
	mux.HandleFunc("GET /requests/{id}/request.json", u.rawRequest)
	mux.HandleFunc("GET /requests/{id}/sent.json", u.rawSent)
	mux.HandleFunc("GET /requests/{id}/response.txt", u.rawResponse)
	mux.HandleFunc("GET /api/limits", u.limitsAPI)
	mux.HandleFunc("POST /api/profiles/{name}/activate", u.profileActivateAPI)
	mux.HandleFunc("POST /settings/profiles", u.profileCreate)
	mux.HandleFunc("POST /settings/profiles/activate", u.profileActivate)
	mux.HandleFunc("POST /settings/profiles/delete", u.profileDelete)
	mux.HandleFunc("POST /settings/pool-settings", u.settingsPoolSave)
	mux.HandleFunc("POST /settings/claude-proxy", u.settingsClaudeProxy)
	mux.HandleFunc("POST /settings/probe", u.settingsProbe)
	mux.HandleFunc("POST /settings/route", u.settingsRoute)
	mux.HandleFunc("POST /settings/models", u.settingsModels)
	mux.HandleFunc("GET /settings/provider", u.settingsProvider)
	mux.HandleFunc("POST /settings/providers", u.settingsProviders)
	mux.HandleFunc("POST /settings/codex/import", u.settingsCodexImport)
	mux.HandleFunc("POST /settings/codex/login", u.settingsCodexLogin)
	mux.HandleFunc("GET /settings/codex/status", u.settingsCodexStatus)
	mux.HandleFunc("GET /settings/codex/usage", u.settingsCodexUsage)
	mux.HandleFunc("POST /settings/codex/usage", u.settingsCodexUsage)
	mux.HandleFunc("POST /settings/pools", u.settingsPools)
	mux.HandleFunc("POST /settings/refresh-models", u.settingsRefreshModels)
	mux.HandleFunc("GET /settings/pool-add", u.settingsPoolAdd)
	return webui.Secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && u.life != nil && !u.life.writesSharedState() {
			webui.Error(w, http.StatusServiceUnavailable, "router is not active")
			return
		}
		if r.Method == http.MethodPost && !sameOriginPost(r) {
			webui.Error(w, http.StatusForbidden, "forbidden")
			return
		}
		// Legacy fragment callers retain their tested rendering contract. Browser
		// navigation always receives the embedded application.
		if r.Method == http.MethodGet && r.Header.Get("HX-Request") == "true" {
			switch r.URL.Path {
			case "/":
				u.index(w, r)
				return
			case "/requests":
				u.list(w, r)
				return
			case "/settings":
				u.settings(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/requests/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/requests/"), "/") {
				r.SetPathValue("id", strings.TrimPrefix(r.URL.Path, "/requests/"))
				u.detail(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
}

// render executes a template into a buffer first: a runtime template error
// becomes a 500 with the message instead of a half-rendered page.
func (u *uiServer) render(w http.ResponseWriter, name string, data any) {
	if api, ok := w.(*actionResponse); ok {
		var err error
		message := "Изменения применены"
		switch v := data.(type) {
		case settingsView:
			if v.Err != "" {
				err = fmt.Errorf("%s", v.Err)
			}
			if v.Flash != "" {
				message = v.Flash
			}
		case providerRow:
			if !v.Probe.OK {
				err = fmt.Errorf("%s", v.Probe.Msg)
			}
			message = "Подключение проверено"
		case claudeProxyView:
			if v.Error != "" {
				err = fmt.Errorf("%s", v.Error)
			}
			message = v.Flash
		case codexUsageView:
			if v.Error != "" {
				err = fmt.Errorf("%s", v.Error)
			}
			message = "Лимиты обновлены"
		}
		api.complete(err, message)
		return
	}
	var buf bytes.Buffer
	if err := u.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("ui template %s: %v", name, err)
		http.Error(w, "template "+name+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes()) // the client may be gone
}

// ---- pages and partials ----

type ctxArgs struct {
	Ctx     *ctxView
	Headers map[string]string
	Q       string
}

type pageView struct {
	Settings *settingsView
	Q        string
	Route    string
	Model    string
}

func (u *uiServer) index(w http.ResponseWriter, r *http.Request) {
	u.render(w, "layout", pageView{Q: r.URL.Query().Get("q")})
}

type statusView struct {
	C         config
	Uptime    string
	Total     int
	Cloud     int
	Local     int
	Errors    int
	Pending   int
	AvgCloud  string
	AvgLocal  string
	Probe     probeResult
	StoreLen  int
	StoreMax  int
	Trimmed   int
	Active    string // provider/model the next local request goes to
	ActiveURL string
	Preferred string
	NModels   int
	Cooling   int
}

func (u *uiServer) status(w http.ResponseWriter, r *http.Request) {
	v := statusView{C: u.cs.Get(), Uptime: fmtDur(time.Since(u.started))}
	v.StoreLen, v.StoreMax = u.st.Size()
	var dc, dl time.Duration
	var nc, nl int
	for _, rec := range u.st.List() {
		v.Total++
		switch rec.Route {
		case "cloud":
			v.Cloud++
		case "local":
			v.Local++
		}
		if !rec.Done() {
			v.Pending++
			continue
		}
		if rec.Failed() {
			v.Errors++
		}
		if rec.TrimBefore != rec.TrimAfter {
			v.Trimmed++
		}
		if rec.Route == "cloud" {
			dc += rec.Duration()
			nc++
		} else {
			dl += rec.Duration()
			nl++
		}
	}
	if nc > 0 {
		v.AvgCloud = fmtDur(dc / time.Duration(nc))
	}
	if nl > 0 {
		v.AvgLocal = fmtDur(dl / time.Duration(nl))
	}
	u.render(w, "status", v)
}

type listItem struct {
	R       *history.Record
	Chars   int
	Msgs    int
	Matches int
	Preview string // last user turn of the request
	Answer  string // first text of the response
	Sel     bool
}

// sessionGroup is one Claude Code session: the records sharing a session id,
// newest first, plus the totals the header shows.
type sessionGroup struct {
	ID     string
	Items  []listItem
	First  time.Time
	Last   time.Time
	Local  int
	Cloud  int
	Errors int
}

func (g sessionGroup) Short() string {
	if g.ID == "" {
		return "без сессии"
	}
	if len(g.ID) > 8 {
		return g.ID[:8]
	}
	return g.ID
}

type listView struct {
	Groups   []*sessionGroup
	Q        string
	Session  string // the one session shown, "" for all
	Sessions int
	Total    int
	Shown    int
}

func (u *uiServer) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	re := searchRegexp(q.Get("q"))
	route := q.Get("route")
	model := strings.ToLower(strings.TrimSpace(q.Get("model")))
	sel := q.Get("sel")
	session := q.Get("session")
	limit := atoiOr(q.Get("limit"), 100)

	v := listView{Q: q.Get("q"), Session: session}
	groups := map[string]*sessionGroup{}
	shown := 0
	for _, rec := range u.st.List() {
		v.Total++
		if route != "" && rec.Route != route {
			continue
		}
		if session != "" && rec.Session != session {
			continue
		}
		if model != "" && !strings.Contains(strings.ToLower(rec.Model), model) {
			continue
		}
		it := listItem{R: rec, Sel: rec.ID == sel}
		it.Chars, it.Msgs, it.Preview = quickSummary(rec.ReqBody)
		if rec.Resp != nil {
			it.Answer = firstLine(rec.Resp.Text())
		}
		if re != nil {
			it.Matches = len(re.FindAllIndex(rec.ReqBody, -1))
			if rec.Resp != nil {
				it.Matches += len(re.FindAllStringIndex(rec.Resp.Text(), -1))
			}
			if it.Matches == 0 {
				continue
			}
		}
		if shown >= limit {
			continue // keep counting Total, stop collecting
		}
		g := groups[rec.Session]
		if g == nil {
			g = &sessionGroup{ID: rec.Session, Last: rec.Start}
			groups[rec.Session] = g
			v.Groups = append(v.Groups, g) // records are newest first, so groups are too
		}
		g.Items = append(g.Items, it)
		g.First = rec.Start
		if rec.Route == "local" {
			g.Local++
		} else {
			g.Cloud++
		}
		if rec.Failed() {
			g.Errors++
		}
		shown++
	}
	v.Shown = shown
	v.Sessions = len(v.Groups)
	u.render(w, "list", v)
}

func (u *uiServer) clear(w http.ResponseWriter, r *http.Request) {
	u.st.Clear()
	u.render(w, "list", listView{})
}

type detailView struct {
	R        *history.Record
	View     string
	Q        string
	Ctx      *ctxView
	Sent     *ctxView
	Resp     *history.Response
	RespHTML []respBlockView
	RawReq   string
	RawSent  string
	RawResp  string
	Usage    []kv
	Params   []kv
}

type respBlockView struct {
	history.Block
	HTML  template.HTML
	Chars int
}

func (u *uiServer) detail(w http.ResponseWriter, r *http.Request) {
	rec := u.st.Get(r.PathValue("id"))
	if rec == nil {
		http.Error(w, "нет такой записи (буфер перезаписан?)", http.StatusNotFound)
		return
	}
	q := r.URL.Query().Get("q")
	re := searchRegexp(q)
	v := detailView{R: rec, Q: q, View: r.URL.Query().Get("view")}
	if v.View == "" {
		v.View = "structure"
	}
	switch v.View {
	case "structure":
		v.Ctx = buildContext(rec.ReqBody, re)
	case "sent":
		if rec.OpenAIBody != nil {
			v.Sent = buildOpenAIContext(rec.OpenAIBody, re)
		}
	case "response":
		v.Resp = rec.Resp
		if rec.Resp != nil {
			for _, b := range rec.Resp.Blocks {
				text := b.Text
				if b.Type == "tool_use" {
					text = b.Input
				}
				v.RespHTML = append(v.RespHTML, respBlockView{Block: b, HTML: highlight(text, re), Chars: len(text)})
			}
			v.Usage = sortedKV(rec.Resp.Usage)
		}
	case "raw":
		v.RawReq = history.PrettyJSON(rec.ReqBody)
		v.RawSent = history.PrettyJSON(rec.OpenAIBody)
		v.RawResp = string(rec.RespBytes)
	}
	u.render(w, "detail", v)
}

func (u *uiServer) rawRequest(w http.ResponseWriter, r *http.Request) {
	u.serveRaw(w, r, func(rec *history.Record) ([]byte, string) { return rec.ReqBody, "application/json" })
}

func (u *uiServer) rawSent(w http.ResponseWriter, r *http.Request) {
	u.serveRaw(w, r, func(rec *history.Record) ([]byte, string) { return rec.OpenAIBody, "application/json" })
}

func (u *uiServer) rawResponse(w http.ResponseWriter, r *http.Request) {
	u.serveRaw(w, r, func(rec *history.Record) ([]byte, string) { return rec.RespBytes, "text/plain; charset=utf-8" })
}

func (u *uiServer) serveRaw(w http.ResponseWriter, r *http.Request, pick func(*history.Record) ([]byte, string)) {
	rec := u.st.Get(r.PathValue("id"))
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	body, ct := pick(rec)
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(body) // the client may be gone
}

// ---- settings ----

type settingsView struct {
	ReloadErrors  []reloadFailure
	ActiveProfile string
	ProfileNames  []string
	ClaudeProxy   claudeProxyView
	C             config
	Flash, Err    string
	Models        []candidate
	Providers     []providerRow
	Pick          *providerRow
	Logins        []codexLoginView
	Pools         []poolRow
	Routes        []routeRow
	Families      []routeRow
	Catalog       modelCatalog
	AllModels     []localModel
	Efforts       []string
	Limits        limits.View
}

type routeRow struct {
	Profile              string
	Model, Label, Family string
	IsFamily             bool
	Choices              []routeChoice
	Pools                []poolRow
	Targets              []directTargetRow
}
type directTargetRow struct {
	Key     string
	Efforts []string
}
type routeChoice struct {
	Effort      string
	Destination string
	Inherited   string
}

var anthropicModels = []string{"claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "claude-fable-5", "claude-sonnet-5", "claude-haiku-4-5", "claude-haiku-4-5-20251001"}

type poolRow struct {
	Profile   string
	Settings  poolSettings
	Name      string
	Keys      []poolKeyRow
	Available []localModel
	Add       poolAddView
	Uses      int
}

type poolKeyRow struct {
	Key, Effort string
	Options     []string
	Unconfirmed bool
	First, Last bool
	Stat        modelStat
}

type codexLoginView struct {
	Provider  string
	Connected bool
	Pending   bool
	Error     string
}

// codexLoginViewFor shows one connection's login; the browser flow's state
// belongs only to the connection it was started for.
func (u *uiServer) codexLoginViewFor(p provider) codexLoginView {
	v := codexLoginView{Provider: p.Name}
	store, err := codexStoreFor(p)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Connected = store.connected()
	u.oauthMu.Lock()
	defer u.oauthMu.Unlock()
	if u.oauthTarget.Name == p.Name && u.oauthTarget.AuthID == p.AuthID {
		v.Pending, v.Error = u.oauthStatus == "pending", u.oauthError
	}
	if v.Error == "" {
		v.Error = store.authStatus()
	}
	return v
}

// codexProvider resolves the connection a /settings/codex/* action names,
// checked against the config in force now.
func (u *uiServer) codexProvider(r *http.Request) (provider, *codexAuthStore, error) {
	name := strings.TrimSpace(r.FormValue("provider"))
	p, ok := u.cs.Get().Local.Provider(name)
	if !ok || p.Type != "codex" {
		return provider{}, nil, fmt.Errorf("подключение Codex %q не найдено", name)
	}
	s, err := codexStoreFor(p)
	return p, s, err
}

// providerRow is one provider with what the UI knows about it.
type providerRow struct {
	P       provider
	KeySet  bool
	NModels int
	Probe   probeResult
	Addable []probeModel // models the endpoint lists that are not configured yet, after Filter
	Total   int          // addable before Filter
	Facets  []facet      // filters with the picked values
	Filter  modelFilter
	Fresh   bool // a form for a provider that does not exist yet
}

func (u *uiServer) settingsView(contexts ...context.Context) settingsView {
	ctx := context.Background()
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	c := u.cs.Get()
	v := settingsView{ReloadErrors: u.cs.ReloadFailures(), ActiveProfile: c.Local.ActiveProfile, ClaudeProxy: u.claudeProxyView(), Catalog: c.Local.Catalog, C: c, Models: u.ranked(c), AllModels: c.Local.Models, Efforts: conf.ProviderEfforts, Limits: u.limits.View(time.Now())}
	for name := range c.Local.Profiles {
		v.ProfileNames = append(v.ProfileNames, name)
	}
	sort.Strings(v.ProfileNames)
	info := map[string]probeModel{}
	for _, p := range c.Local.Providers {
		if p.Type == "codex" {
			v.Logins = append(v.Logins, u.codexLoginViewFor(p))
		}
		row := u.providerRow(c, p, false, modelFilter{}, ctx)
		v.Providers = append(v.Providers, row)
		for _, m := range row.Probe.Info {
			info[p.Name+"/"+m.ID] = m
		}
	}
	names := make([]string, 0, len(c.Local.ModelPools))
	for name := range c.Local.ModelPools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		targets := c.Local.ModelPools[name]
		row := poolRow{Profile: c.Local.ActiveProfile, Name: name, Settings: c.PoolSettings(name)}
		present := map[string]bool{}
		for i, target := range targets {
			options := modelEffortOptions(c.Local, target.Model, info)
			row.Keys = append(row.Keys, poolKeyRow{Key: target.Model, Effort: target.Effort, Options: options, Unconfirmed: target.Effort != "" && target.Effort != conf.PoolRequestEffort && !slices.Contains(options, target.Effort), First: i == 0, Last: i == len(targets)-1, Stat: u.hl.snapshot(target.Model)})
			present[target.Model] = true
		}
		for _, m := range c.Local.Models {
			if !present[m.Key()] {
				row.Available = append(row.Available, m)
			}
		}
		for _, efforts := range c.Local.AllRouteRules() {
			for _, route := range efforts {
				if route.Mode == "pool" && route.Pool == name {
					row.Uses++
				}
			}
		}
		row.Add = u.poolAddView(c.Local, name, "", info)
		v.Pools = append(v.Pools, row)
	}
	var directTargets []directTargetRow
	for _, model := range c.Local.Models {
		directTargets = append(directTargets, directTargetRow{Key: model.Key(), Efforts: modelEffortOptions(c.Local, model.Key(), info)})
	}
	models := map[string]bool{}
	catalog := c.Local.Catalog.Anthropic
	if len(catalog) == 0 {
		catalog = anthropicModels
	}
	for _, model := range catalog {
		models[model] = true
	}
	for model := range c.Local.Routes {
		models[model] = true
	}
	for _, rec := range u.st.List() {
		if rec.Model != "" {
			models[rec.Model] = true
		}
	}
	families := map[string]bool{}
	for model := range models {
		if family := conf.ClaudeFamily(model); family != "" {
			families[family] = true
		}
	}
	for family := range c.Local.FamilyRoutes {
		families[family] = true
	}
	familyNames := make([]string, 0, len(families))
	for family := range families {
		familyNames = append(familyNames, family)
	}
	sort.Strings(familyNames)
	for _, family := range familyNames {
		row := routeRow{Profile: c.Local.ActiveProfile, Model: family, Label: strings.ToUpper(family[:1]) + family[1:], IsFamily: true, Pools: v.Pools, Targets: directTargets}
		for _, effort := range conf.ClaudeEfforts {
			route, ok := c.Local.FamilyRoutes[family][effort]
			if !ok {
				route.Mode = "disabled"
			}
			row.Choices = append(row.Choices, routeChoice{Effort: effort, Destination: routeDestination(route)})
		}
		v.Families = append(v.Families, row)
	}
	ids := make([]string, 0, len(models))
	for model := range models {
		ids = append(ids, model)
	}
	sort.Strings(ids)
	for _, model := range ids {
		family := conf.ClaudeFamily(model)
		row := routeRow{Profile: c.Local.ActiveProfile, Model: model, Label: model, Family: family, Pools: v.Pools, Targets: directTargets}
		for _, effort := range conf.ClaudeEfforts {
			route, explicit := c.Local.Routes[model][effort]
			choice := routeChoice{Effort: effort, Destination: "disabled"}
			if explicit {
				choice.Destination = routeDestination(route)
			} else if family != "" {
				choice.Destination = "inherit"
			}
			inherited, ok := c.Local.FamilyRoutes[family][effort]
			choice.Inherited = "не настроено"
			if ok && inherited.Mode == "anthropic" {
				choice.Inherited = "Anthropic"
			}
			if ok && inherited.Mode == "pool" {
				choice.Inherited = "пул " + inherited.Pool
			}
			if ok && inherited.Mode == "model" {
				choice.Inherited = inherited.Model
				if inherited.Effort != "" {
					choice.Inherited += " / " + inherited.Effort
				}
			}
			row.Choices = append(row.Choices, choice)
		}
		v.Routes = append(v.Routes, row)
	}
	return v
}

func (u *uiServer) settings(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "" {
		v := u.settingsView(r.Context())
		u.render(w, "layout", pageView{Settings: &v})
		return
	}
	u.render(w, "settings", u.settingsView(r.Context()))
}

func (u *uiServer) settingsPoolSave(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("profile") != "" && r.FormValue("profile") != u.cs.Get().Local.ActiveProfile {
		u.renderSettingsResult(w, fmt.Errorf("активный профиль изменился; обновите страницу"), "", r.Context())
		return
	}
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm() // a malformed form reads as empty fields
	settings, err := conf.ParsePoolSettings(settingsInput{
		MaxInputChars: r.FormValue("max_input_chars"),
		Failover:      r.FormValue("failover"),
		FirstByte:     r.FormValue("first_byte"),
		ProbeEvery:    r.FormValue("probe_every"),
		Type:          r.FormValue("type"),
	})
	if err == nil {
		err = u.cs.SavePoolSettings(r.FormValue("name"), settings, r.FormValue("profile"))
	}
	u.renderSettingsResult(w, err, "Настройки пула сохранены и применены", r.Context())
}

func disableDirectRoutes(l *localSetup, removed func(string) bool) {
	for _, rules := range l.AllRouteRules() {
		for effort, route := range rules {
			if route.Mode == "model" && removed(route.Model) {
				rules[effort] = modelRoute{Mode: "disabled"}
			}
		}
	}
}

func routeDestination(route modelRoute) string {
	if route.Mode == "pool" {
		return "pool:" + route.Pool
	}
	if route.Mode == "model" {
		return "model:" + route.Model + ":" + route.Effort
	}
	return route.Mode
}

// settingsRoute saves family defaults or explicit version overrides.
func (u *uiServer) settingsRoute(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("profile") != "" && r.FormValue("profile") != u.cs.Get().Local.ActiveProfile {
		u.renderSettingsResult(w, fmt.Errorf("активный профиль изменился; обновите страницу"), "", r.Context())
		return
	}
	_ = r.ParseForm() // a malformed form reads as empty fields
	l := u.cs.Get().Local.Clone()
	if l.Routes == nil {
		l.Routes = map[string]map[string]modelRoute{}
	}
	model := strings.TrimSpace(r.FormValue("model"))
	familyScope := r.FormValue("scope") == "family"
	choices := map[string]modelRoute{}
	// «На все effort» sends one destination for every level. A model target
	// without a level takes the same-named one. A level missing from its catalog
	// keeps the row's choice and is named; other errors reject the save below.
	all := r.FormValue("all")
	rest, isModel := strings.CutPrefix(all, "model:")
	allKey, perLevel := strings.CutSuffix(rest, ":")
	perLevel = perLevel && isModel
	var lacking []string
	for _, effort := range conf.ClaudeEfforts {
		dest := r.FormValue(effort)
		switch {
		case perLevel && effort != "default" && strings.Contains(fmt.Sprint(u.validateTargetEffort(l, allKey, effort, r.Context())), "не подтверждён каталогом"):
			lacking = append(lacking, effort)
		case perLevel && effort != "default":
			dest = all + effort
		case all != "":
			dest = all
		}
		if dest == "inherit" || !familyScope && dest == "" && conf.ClaudeFamily(model) != "" {
			continue
		}
		route := modelRoute{Mode: dest}
		if dest == "" {
			route.Mode = "disabled"
		}
		if strings.HasPrefix(dest, "pool:") {
			route = modelRoute{Mode: "pool", Pool: strings.TrimPrefix(dest, "pool:")}
		}
		if strings.HasPrefix(dest, "model:") {
			target := strings.TrimPrefix(dest, "model:")
			if i := strings.LastIndexByte(target, ':'); i >= 0 {
				route = modelRoute{Mode: "model", Model: target[:i], Effort: target[i+1:]}
			} else {
				route = modelRoute{Mode: "model"}
			}
		}
		if route.Mode == "model" {
			if err := u.validateTargetEffort(l, route.Model, route.Effort, r.Context()); err != nil {
				u.renderSettingsResult(w, err, "", r.Context())
				return
			}
		}
		choices[effort] = route
	}
	if familyScope {
		if l.FamilyRoutes == nil {
			l.FamilyRoutes = map[string]map[string]modelRoute{}
		}
		l.FamilyRoutes[model] = choices
	} else {
		l.Routes[model] = choices
	}
	message := "Маршруты сохранены: " + model
	if len(lacking) > 0 {
		message += fmt.Sprintf(". У %s нет effort %s — эти строки не изменены", allKey, strings.Join(lacking, ", "))
	}
	u.renderSettingsResult(w, u.cs.Update(conf.Replace(l, r.FormValue("profile"))), message, r.Context())
}

func (u *uiServer) renderSettingsResult(w http.ResponseWriter, err error, message string, contexts ...context.Context) {
	if api, ok := w.(*actionResponse); ok {
		api.complete(err, message)
		return
	}
	v := u.settingsView(contexts...)
	if err != nil {
		v.Err = err.Error()
	} else {
		v.Flash = message
	}
	u.render(w, "settings", v)
}

// settingsProbe re-probes one provider and re-renders its pane.
func (u *uiServer) settingsProbe(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm() // a malformed form reads as empty fields
	c := u.cs.Get()
	p, ok := c.Local.Provider(strings.TrimSpace(r.FormValue("provider")))
	if !ok {
		u.render(w, "settings", u.settingsView(r.Context()))
		return
	}
	u.render(w, "provider", u.providerRow(c, p, true, modelFilter{}, r.Context()))
}

// settingsProvider renders the pane for the provider chosen in the picker:
// its probe, the models it offers that are not configured yet, and its form.
// An unknown or empty name is the form for a new provider.
func (u *uiServer) settingsProvider(w http.ResponseWriter, r *http.Request) {
	c := u.cs.Get()
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if p, ok := c.Local.Provider(name); ok {
		u.render(w, "provider", u.providerRow(c, p, r.URL.Query().Get("filter") == "", filterFrom(r.URL.Query()), r.Context()))
		return
	}
	u.render(w, "provider", providerRow{Fresh: true, P: provider{BaseURL: "http://127.0.0.1:1234/v1"}})
}

// settingsProviders adds, edits or removes a provider. Removing one drops its
// models too; affected pools remain empty and reject requests until configured.
func (u *uiServer) settingsProviders(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm() // a malformed form reads as empty fields
	c := u.cs.Get()
	l := c.Local.Clone()
	name := strings.TrimSpace(r.FormValue("name"))
	orig := strings.TrimSpace(r.FormValue("orig")) // name before an edit
	var err error
	flash := ""
	switch op := r.FormValue("op"); op {
	case "add":
		if _, dup := l.Provider(name); dup {
			err = fmt.Errorf("провайдер %q уже есть", name)
			break
		}
		p := provider{Name: name, DisplayName: strings.TrimSpace(r.FormValue("display_name")), Type: r.FormValue("type"), BaseURL: r.FormValue("base_url"), APIKey: r.FormValue("api_key")}
		if p.Type == "codex" {
			p.BaseURL, p.APIKey, p.AuthID = conf.CodexBaseURL, "", conf.NewCodexAuthID()
		}
		l.Providers = append(l.Providers, p)
		flash = "Провайдер добавлен: " + name
	case "update":
		idx := -1
		for i, p := range l.Providers {
			if p.Name == orig {
				idx = i
			}
		}
		if idx < 0 {
			err = fmt.Errorf("провайдер %q не найден", orig)
			break
		}
		if _, dup := l.Provider(name); dup && name != orig {
			err = fmt.Errorf("провайдер %q уже есть", name)
			break
		}
		p := &l.Providers[idx]
		if p.Type == "codex" && name != orig {
			err = fmt.Errorf("подключение Codex нельзя переименовать: удалите и добавьте заново со входом")
			break
		}
		p.Name = name
		if r.Form.Has("display_name") {
			p.DisplayName = strings.TrimSpace(r.FormValue("display_name"))
		}
		if p.Type != "codex" {
			if r.Form.Has("base_url") {
				p.BaseURL = r.FormValue("base_url")
			}
			switch {
			case r.FormValue("clear_key") == "1":
				p.APIKey = ""
			case r.FormValue("api_key") != "":
				p.APIKey = r.FormValue("api_key")
			}
		}
		for i := range l.Models {
			if l.Models[i].Provider == orig {
				l.Models[i].Provider = name
			}
		}
		for pattern, pool := range l.ModelPools {
			for i, key := range pool {
				if strings.HasPrefix(key.Model, orig+"/") {
					pool[i].Model = name + strings.TrimPrefix(key.Model, orig)
				}
			}
			l.ModelPools[pattern] = pool
		}
		if name != orig {
			for _, rules := range l.AllRouteRules() {
				for effort, route := range rules {
					if route.Mode == "model" && strings.HasPrefix(route.Model, orig+"/") {
						route.Model = name + strings.TrimPrefix(route.Model, orig)
						rules[effort] = route
					}
				}
			}
		}
		if name != orig {
			l.RenameInactiveProvider(orig, name)
		}
		if name != orig {
			if entry, ok := l.Catalog.Providers[orig]; ok {
				l.Catalog.Providers[name] = entry
				delete(l.Catalog.Providers, orig)
			}
			if ids, ok := l.Catalog.CodexSeen[orig]; ok {
				l.Catalog.CodexSeen[name] = ids
				delete(l.Catalog.CodexSeen, orig)
			}
		}
		flash = "Провайдер сохранён: " + name
	case "remove":
		var keepP []provider
		for _, p := range l.Providers {
			if p.Name != name {
				keepP = append(keepP, p)
			}
		}
		if len(keepP) == len(l.Providers) {
			err = fmt.Errorf("провайдер %q не найден", name)
			break
		}
		l.Providers = keepP
		var keepM []localModel
		for _, m := range l.Models {
			if m.Provider != name {
				keepM = append(keepM, m)
			}
		}
		l.Models = keepM
		for pattern, pool := range l.ModelPools {
			kept := pool[:0:0]
			for _, key := range pool {
				if !strings.HasPrefix(key.Model, name+"/") {
					kept = append(kept, key)
				}
			}
			l.ModelPools[pattern] = kept
		}
		disableDirectRoutes(&l, func(key string) bool { return strings.HasPrefix(key, name+"/") })
		delete(l.Catalog.Providers, name)
		delete(l.Catalog.CodexSeen, name)
		flash = "Провайдер удалён: " + name
	default:
		err = fmt.Errorf("unknown op %q", op)
	}
	if err == nil {
		err = u.cs.Update(conf.Replace(l, ""))
	}
	if api, ok := w.(*actionResponse); ok {
		api.complete(err, flash)
		return
	}
	v := u.settingsView(r.Context())
	if err != nil {
		v.Err = err.Error()
	} else {
		n := u.cs.Get()
		v.Flash = flash
		log.Printf("ui: providers: %s", n.Local.Summary())
		if p, ok := n.Local.Provider(name); ok {
			row := u.providerRow(n, p, true, modelFilter{}, r.Context())
			v.Pick = &row
		}
	}
	u.render(w, "settings", v)
}

func (u *uiServer) settingsCodexImport(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_, store, err := u.codexProvider(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err = store.importFromCLI()
	if err == nil {
		u.probeMu.Lock()
		u.probe = nil
		u.probeMu.Unlock()
	}
	if api, ok := w.(*actionResponse); ok {
		api.complete(err, "Вход Codex CLI обновлён")
		return
	}
	v := u.settingsView(r.Context())
	if err != nil {
		v.Err = err.Error()
	} else {
		v.Flash = "Вход Codex CLI обновлён"
	}
	u.render(w, "settings", v)
}

func (u *uiServer) settingsCodexLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p, store, err := u.codexProvider(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if codextesttransport.DisableBackground() {
		http.Error(w, "synthetic Codex build cannot start browser login", http.StatusServiceUnavailable)
		return
	}
	u.oauthMu.Lock()
	if u.oauthStatus == "pending" && u.oauthFlow != nil {
		url, target := u.oauthFlow.URL, u.oauthTarget
		u.oauthMu.Unlock()
		if target.Name != p.Name || target.AuthID != p.AuthID {
			http.Error(w, fmt.Sprintf("идёт вход для %q; дождитесь его окончания", target.Name), http.StatusConflict)
			return
		}
		http.Redirect(w, r, url, http.StatusSeeOther)
		return
	}
	flow, err := startCodexBrowserFlow(r.Context(), codexLoginAddr, store.issuer)
	if err != nil {
		u.oauthStatus, u.oauthError = "failed", err.Error()
		u.oauthMu.Unlock()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	u.oauthFlow, u.oauthTarget, u.oauthStatus, u.oauthError = flow, p, "pending", ""
	u.oauthMu.Unlock()
	go func() {
		err := store.finishBrowserFlow(context.Background(), flow, func() bool {
			now, ok := u.cs.Get().Local.Provider(p.Name)
			return ok && now.Type == "codex" && now.AuthID == p.AuthID
		})
		u.oauthMu.Lock()
		if u.oauthFlow == flow {
			u.oauthFlow = nil
			if err != nil {
				u.oauthStatus, u.oauthError = "failed", err.Error()
			} else {
				u.oauthStatus, u.oauthError = "complete", ""
			}
		}
		u.oauthMu.Unlock()
		if err == nil {
			u.probeMu.Lock()
			u.probe = nil
			u.probeMu.Unlock()
		}
	}()
	http.Redirect(w, r, flow.URL, http.StatusSeeOther)
}

func sameOriginPost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host && (u.Scheme == "http" || u.Scheme == "https")
}

func (u *uiServer) settingsCodexStatus(w http.ResponseWriter, r *http.Request) {
	p, _, err := u.codexProvider(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.render(w, "codex-login", u.codexLoginViewFor(p))
}

func (u *uiServer) settingsPools(w http.ResponseWriter, r *http.Request) {
	c := u.cs.Get()
	if r.FormValue("profile") != "" && r.FormValue("profile") != c.Local.ActiveProfile {
		u.renderSettingsResult(w, fmt.Errorf("активный профиль изменился; обновите страницу"), "", r.Context())
		return
	}
	_ = r.ParseForm() // a malformed form reads as empty fields
	l := c.Local.Clone()
	if l.ModelPools == nil {
		l.ModelPools = map[string][]poolTarget{}
	}
	name, key, op := strings.TrimSpace(r.FormValue("name")), r.FormValue("key"), r.FormValue("op")
	pool, exists := l.ModelPools[name]
	var err error
	switch op {
	case "default":
		if _, supplied := r.Form["name"]; !supplied {
			err = fmt.Errorf("укажите пул или отключите правило")
		} else if name != "" && !exists {
			err = fmt.Errorf("пул %q не найден", name)
		} else {
			l.DefaultPool = name
		}
	case "create":
		if exists {
			err = fmt.Errorf("пул %q уже существует", name)
		} else {
			l.ModelPools[name] = []poolTarget{}
		}
	case "clone":
		source := r.FormValue("source")
		members, found := l.ModelPools[source]
		switch {
		case exists:
			err = fmt.Errorf("пул %q уже существует", name)
		case !found:
			err = fmt.Errorf("исходный пул %q не найден", source)
		default:
			l.ModelPools[name] = append([]poolTarget{}, members...)
			for i, member := range members {
				l.ModelPools[name][i] = member.Clone()
			}
			if l.PoolSettings == nil {
				l.PoolSettings = map[string]poolSettings{}
			}
			l.PoolSettings[name] = c.PoolSettings(source)
		}
	case "delete":
		if l.DefaultPool == name {
			err = fmt.Errorf("пул %q используется по умолчанию; сначала выберите другой пул или отключите правило", name)
		}
		for _, efforts := range l.AllRouteRules() {
			for _, route := range efforts {
				if route.Mode == "pool" && route.Pool == name {
					err = fmt.Errorf("пул %q используется: сначала измените его маршруты", name)
				}
			}
		}
		if err == nil {
			delete(l.ModelPools, name)
			delete(l.PoolSettings, name)
		}
	case "add":
		if !exists {
			err = fmt.Errorf("пул не найден")
		} else {
			l.ModelPools[name] = append(pool, poolTarget{Model: key, Effort: r.FormValue("effort")})
		}
	case "remove", "up", "down", "effort", "mapping":
		idx := -1
		for i, target := range pool {
			if target.Model == key {
				idx = i
				break
			}
		}
		if idx < 0 {
			err = fmt.Errorf("модель не входит в пул")
			break
		}
		switch op {
		case "remove":
			l.ModelPools[name] = append(pool[:idx:idx], pool[idx+1:]...)
		case "up":
			if idx > 0 {
				pool[idx-1], pool[idx] = pool[idx], pool[idx-1]
			}
		case "down":
			if idx+1 < len(pool) {
				pool[idx], pool[idx+1] = pool[idx+1], pool[idx]
			}
		case "effort":
			pool[idx].Effort = r.FormValue("effort")
		case "mapping":
			mapping := map[string]string{}
			for _, source := range conf.ClaudeEfforts {
				if _, ok := r.Form[source]; !ok {
					err = fmt.Errorf("не указано соответствие для %s", source)
					break
				}
				value := r.FormValue(source)
				if value == "inherit" {
					continue
				}
				if value != conf.PoolRequestEffort {
					if err = u.validateTargetEffort(l, key, value, r.Context()); err != nil {
						break
					}
				}
				mapping[source] = value
			}
			if err == nil {
				pool[idx].EffortMap = mapping
			}
		}
	default:
		err = fmt.Errorf("неизвестная операция")
	}
	if err == nil && (op == "add" || op == "effort") && r.FormValue("effort") != conf.PoolRequestEffort {
		err = u.validateTargetEffort(l, key, r.FormValue("effort"), r.Context())
	}
	if err == nil {
		err = u.cs.Update(conf.Replace(l, r.FormValue("profile")))
	}
	u.renderSettingsResult(w, err, "Пул сохранён", r.Context())
}

func (u *uiServer) validateTargetEffort(l localSetup, key, effort string, contexts ...context.Context) error {
	if effort == "" {
		return nil
	}
	for _, model := range l.Models {
		if model.Key() != key {
			continue
		}
		p, _ := l.Provider(model.Provider)
		probe := u.probeProvider(p, false, contexts...)
		if p.Type == "codex" && probe.OK && !slices.Contains(probe.Models, model.Model) {
			return fmt.Errorf("%s отсутствует в текущем каталоге Codex", key)
		}
		info := map[string]probeModel{}
		for _, m := range probe.Info {
			info[p.Name+"/"+m.ID] = m
		}
		if slices.Contains(modelEffortOptions(l, key, info), effort) {
			return nil
		}
		return fmt.Errorf("%s: effort %q не подтверждён каталогом модели. Обновите модели или выберите доступный уровень.", key, effort)
	}
	return fmt.Errorf("модель %q не настроена", key)
}

// Codex levels come exclusively from this model's catalog, including the last
// successful persisted catalog while temporarily offline. Never use a union of
// unrelated models' capabilities for subscription models.
func modelEffortOptions(l localSetup, key string, info map[string]probeModel) []string {
	for _, model := range l.Models {
		if model.Key() != key {
			continue
		}
		p, _ := l.Provider(model.Provider)
		if m, ok := info[key]; ok && (p.Type == "codex" || len(m.Efforts) > 0) {
			return m.Efforts
		}
		if p.Type != "codex" {
			return conf.ProviderEfforts
		}
		for _, m := range l.Catalog.Providers[p.Name].Models {
			if m.ID == model.Model {
				return m.Efforts
			}
		}
		return nil
	}
	return nil
}

type poolAddView struct {
	Profile   string
	Name, Key string
	Models    []localModel
	Options   []string
}

func (u *uiServer) poolAddView(l localSetup, name, key string, info map[string]probeModel) poolAddView {
	v := poolAddView{Profile: l.ActiveProfile, Name: name}
	present := map[string]bool{}
	for _, m := range l.ModelPools[name] {
		present[m.Model] = true
	}
	for _, m := range l.Models {
		if !present[m.Key()] {
			v.Models = append(v.Models, m)
			if key == m.Key() {
				v.Key = key
			}
		}
	}
	if v.Key == "" && len(v.Models) > 0 {
		v.Key = v.Models[0].Key()
	}
	v.Options = modelEffortOptions(l, v.Key, info)
	return v
}

func (u *uiServer) settingsPoolAdd(w http.ResponseWriter, r *http.Request) {
	l := u.cs.Get().Local
	name, key := r.URL.Query().Get("name"), r.URL.Query().Get("key")
	if _, ok := l.ModelPools[name]; !ok {
		http.Error(w, "пул не найден", http.StatusNotFound)
		return
	}
	info := map[string]probeModel{}
	for _, m := range l.Models {
		if m.Key() == key {
			p, _ := l.Provider(m.Provider)
			for _, item := range u.probeProvider(p, false, r.Context()).Info {
				info[p.Name+"/"+item.ID] = item
			}
		}
	}
	u.render(w, "pool-add", u.poolAddView(l, name, key, info))
}

// providerRow probes the provider and lists which of its models can still be added.
func (u *uiServer) providerRow(c config, p provider, force bool, f modelFilter, contexts ...context.Context) providerRow {
	ctx := context.Background()
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	row := providerRow{P: p, KeySet: p.APIKey != "", Probe: u.probeProvider(p, force, ctx), Filter: f}
	for _, m := range c.Local.Models {
		if m.Provider == p.Name {
			row.NModels++
		}
	}
	row.Facets = append([]facet(nil), row.Probe.Facets...)
	for i := range row.Facets {
		row.Facets[i].Picked = f.Pick[row.Facets[i].Key]
	}
	for _, m := range row.Probe.Info {
		if c.Local.HasModel(p.Name + "/" + m.ID) {
			continue
		}
		row.Total++
		if f.keep(m, row.Facets) {
			row.Addable = append(row.Addable, m)
		}
	}
	return row
}

// filterFrom reads the pane's filter form: q, and f.<key>=<value> per facet.
func filterFrom(q url.Values) modelFilter {
	f := modelFilter{Q: q.Get("q"), Pick: map[string]string{}}
	for k, vs := range q {
		if strings.HasPrefix(k, "f.") && len(vs) > 0 {
			f.Pick[k[2:]] = vs[0]
		}
	}
	return f
}

// probeProvider asks a provider for its model list, cached for a few seconds
// per provider so the status strip polling does not hammer it.
func (u *uiServer) probeProvider(p provider, force bool, contexts ...context.Context) probeResult {
	ctx := context.Background()
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	u.probeMu.Lock()
	defer u.probeMu.Unlock()
	if u.probe == nil {
		u.probe = map[string]probeResult{}
	}
	if old, ok := u.probe[p.Name]; ok && !force && old.Base == p.BaseURL && old.Key == p.APIKey && old.AuthID == p.AuthID && time.Since(old.At) < catalogRefreshInterval {
		return old
	}
	res := probeResult{At: time.Now(), Base: p.BaseURL, Key: p.APIKey, AuthID: p.AuthID}
	defer func() { u.probe[p.Name] = res }()
	var deps catalogstartup.Dependencies
	if u.catalog != nil {
		deps = *u.catalog
	}
	body, msg := deps.FetchModels(ctx, conf.CodexBaseURL+"/models?client_version=0.156.0", catalogstartup.ProbeInput{
		Name: p.Name, Kind: p.Type, BaseURL: p.BaseURL, AuthID: p.AuthID, APIKey: p.APIKey,
	}, func(ctx context.Context, req *http.Request) error {
		store, err := codexStoreFor(p)
		if err != nil {
			return err
		}
		return store.authorize(ctx, req)
	})
	if msg != "" {
		res.Msg = msg
		return res
	}
	if p.Type == "codex" {
		var catalog struct {
			Models []struct {
				Slug       string `json:"slug"`
				Name       string `json:"display_name"`
				Visibility string `json:"visibility"`
				Supported  []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		if json.Unmarshal(body, &catalog) != nil {
			res.Msg = "некорректный список моделей Codex"
			return res
		}
		for _, m := range catalog.Models {
			if m.Slug != "" && m.Visibility == "list" {
				info := probeModel{ID: m.Slug, Name: m.Name}
				for _, level := range m.Supported {
					if conf.ValidProviderEffort(level.Effort) {
						info.Efforts = append(info.Efforts, level.Effort)
					}
				}
				res.Info = append(res.Info, info)
			}
		}
	} else {
		res.Info = parseModels(body)
	}
	res.Facets = facetsOf(res.Info)
	for _, m := range res.Info {
		res.Models = append(res.Models, m.ID)
	}
	res.OK = true
	res.Msg = fmt.Sprintf("доступен, моделей: %d", len(res.Models))
	return res
}

// ---- helpers ----

type kv struct{ K, V string }

func sortedKV(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, strconv.Itoa(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].K < out[j].K })
	return out
}

func searchRegexp(q string) *regexp.Regexp {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	return regexp.MustCompile("(?i)" + regexp.QuoteMeta(q))
}

// highlight escapes text and wraps every match in <mark>. Offsets come from the
// regexp, so multi-byte case folding cannot misalign them.
func highlight(text string, re *regexp.Regexp) template.HTML {
	if re == nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(text, -1) {
		b.WriteString(template.HTMLEscapeString(text[last:m[0]]))
		b.WriteString("<mark>")
		b.WriteString(template.HTMLEscapeString(text[m[0]:m[1]]))
		b.WriteString("</mark>")
		last = m[1]
	}
	b.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(b.String())
}

func countMatches(text string, re *regexp.Regexp) int {
	if re == nil {
		return 0
	}
	return len(re.FindAllStringIndex(text, -1))
}

// quickSummary is the cheap read for the list: sizes and the newest user text.
// firstLine is the first non-empty line of a text, for one-line previews.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

func quickSummary(body []byte) (chars, msgs int, preview string) {
	var req struct {
		Messages []anthropicMsg `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return len(body), 0, ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		blocks, _ := decodeBlocks(m.Content)
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				preview = strings.TrimSpace(b.Text)
				break
			}
			if b.Type == "tool_result" {
				preview = "tool_result → " + toolResultText(b.Content)
				break
			}
		}
		if preview != "" {
			break
		}
	}
	preview = strings.Join(strings.Fields(preview), " ")
	if chars := []rune(preview); len(chars) > 140 {
		preview = string(chars[:140]) + "…"
	}
	return len(body), len(req.Messages), preview
}

func fmtChars(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func fmtAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds назад", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm назад", int(d.Minutes()))
	}
	return t.Format("15:04")
}

// ranked is the failover order regardless of whether failover is on, for display.
func (u *uiServer) ranked(c config) []candidate {
	c.Failover = true
	return u.hl.pick(c)
}

// settingsModels handles the one-click actions on the local models table.
// Models are addressed by key (provider/model); add takes provider + model.
func (u *uiServer) settingsModels(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm() // a malformed form reads as empty fields
	c := u.cs.Get()
	l := c.Local.Clone()
	key := strings.TrimSpace(r.FormValue("key"))
	pname := strings.TrimSpace(r.FormValue("provider"))
	var err error
	op := r.FormValue("op")
	local := true // op changes providers.json rather than env or ratings
	switch op {
	case "add":
		for _, id := range conf.SplitList(r.FormValue("model")) {
			m := localModel{Provider: pname, Model: id}

			l.Models = append(l.Models, m)
		}
	case "remove":
		var keep []localModel
		for _, m := range l.Models {
			if m.Key() != key {
				keep = append(keep, m)
			}
		}
		l.Models = keep
		for pattern, pool := range l.ModelPools {
			kept := pool[:0:0]
			for _, member := range pool {
				if member.Model != key {
					kept = append(kept, member)
				}
			}
			l.ModelPools[pattern] = kept
		}
		disableDirectRoutes(&l, func(model string) bool { return model == key })
	case "failover":
		local = false
		err = u.cs.SetFailover(r.FormValue("on") == "1")
	case "reset":
		local = false
		u.hl.reset(key) // "" resets all
	default:
		local = false
		err = fmt.Errorf("unknown op %q", op)
	}
	if err == nil && local {
		err = u.cs.Update(conf.Replace(l, ""))
	}
	if api, ok := w.(*actionResponse); ok {
		api.complete(err, "Каталог моделей обновлён")
		return
	}
	v := u.settingsView(r.Context())
	if err != nil {
		v.Err = err.Error()
	} else {
		n := u.cs.Get()
		log.Printf("ui: local models: %s failover=%v", n.Local.Summary(), n.Failover)
		if p, ok := n.Local.Provider(pname); ok && op == "add" {
			row := u.providerRow(n, p, false, modelFilter{}, r.Context())
			v.Pick = &row
		}
	}
	u.render(w, "settings", v)
}
