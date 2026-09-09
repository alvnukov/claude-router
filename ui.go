package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/*
var uiFS embed.FS

// uiServer is the inspection and control panel. It binds to its own address
// (loopback by default) so nothing about it touches the API path, and it has
// no auth: everything it shows is the traffic of the user running it.
type uiServer struct {
	st      *store
	cs      *configStore
	hl      *health
	tpl     *template.Template
	started time.Time

	probeMu sync.Mutex
	probe   map[string]probeResult // by provider name
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
}

// uiTemplates parses the embedded pages. Kept apart from startUI so a test
// can catch a broken template before launchd does.
func uiTemplates() (*template.Template, error) {
	funcs := template.FuncMap{
		"kb":     fmtChars,
		"fmtnum": fmtNum,
		"dur":    fmtDur,
		"ago":    fmtAgo,
		"pct":    func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		"score":  func(f float64) string { return strconv.FormatFloat(f*100, 'f', 0, 64) + "%" },
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

func startUI(addr string, st *store, cs *configStore, hl *health) {
	tpl := template.Must(uiTemplates())
	u := &uiServer{st: st, cs: cs, tpl: tpl, started: time.Now(), hl: hl}

	static, _ := fs.Sub(uiFS, "ui")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", u.index)
	mux.HandleFunc("GET /status", u.status)
	mux.HandleFunc("GET /requests", u.list)
	mux.HandleFunc("POST /requests/clear", u.clear)
	mux.HandleFunc("GET /requests/{id}", u.detail)
	mux.HandleFunc("GET /requests/{id}/request.json", u.rawRequest)
	mux.HandleFunc("GET /requests/{id}/sent.json", u.rawSent)
	mux.HandleFunc("GET /requests/{id}/response.txt", u.rawResponse)
	mux.HandleFunc("GET /settings", u.settings)
	mux.HandleFunc("POST /settings", u.settingsSave)
	mux.HandleFunc("POST /settings/probe", u.settingsProbe)
	mux.HandleFunc("POST /settings/route", u.settingsRoute)
	mux.HandleFunc("POST /settings/models", u.settingsModels)
	mux.HandleFunc("GET /settings/provider", u.settingsProvider)
	mux.HandleFunc("POST /settings/providers", u.settingsProviders)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("ui: %v", err)
		}
	}()
}

func (u *uiServer) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("ui template %s: %v", name, err)
	}
}

// ---- pages and partials ----

type ctxArgs struct {
	Ctx     *ctxView
	Headers map[string]string
	Q       string
}

type pageView struct {
	Q     string
	Route string
	Model string
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
	v := statusView{C: u.cs.get(), Uptime: fmtDur(time.Since(u.started))}
	v.StoreLen, v.StoreMax = u.st.size()
	v.Preferred = v.C.local.Preferred
	if cands := u.hl.pick(v.C); len(cands) > 0 {
		v.Active = cands[0].Key
		v.ActiveURL = cands[0].Provider.BaseURL
		v.Probe = u.probeProvider(cands[0].Provider, false)
	} else {
		v.Probe = probeResult{Msg: "нет локальных моделей"}
	}
	for _, cand := range u.ranked(v.C) {
		v.NModels++
		if cand.Stat.Cooling() {
			v.Cooling++
		}
	}
	var dc, dl time.Duration
	var nc, nl int
	for _, rec := range u.st.list() {
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
	R       *record
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
	for _, rec := range u.st.list() {
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
		if shown >= limit {
			break
		}
	}
	v.Shown = shown
	v.Sessions = len(v.Groups)
	u.render(w, "list", v)
}

func (u *uiServer) clear(w http.ResponseWriter, r *http.Request) {
	u.st.clear()
	u.render(w, "list", listView{})
}

type detailView struct {
	R        *record
	View     string
	Q        string
	Ctx      *ctxView
	Sent     *ctxView
	Resp     *parsedResponse
	RespHTML []respBlockView
	RawReq   string
	RawSent  string
	RawResp  string
	Usage    []kv
	Params   []kv
}

type respBlockView struct {
	respBlock
	HTML  template.HTML
	Chars int
}

func (u *uiServer) detail(w http.ResponseWriter, r *http.Request) {
	rec := u.st.get(r.PathValue("id"))
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
				v.RespHTML = append(v.RespHTML, respBlockView{respBlock: b, HTML: highlight(text, re), Chars: len(text)})
			}
			v.Usage = sortedKV(rec.Resp.Usage)
		}
	case "raw":
		v.RawReq = prettyJSON(rec.ReqBody)
		v.RawSent = prettyJSON(rec.OpenAIBody)
		v.RawResp = string(rec.RespBytes)
	}
	u.render(w, "detail", v)
}

func (u *uiServer) rawRequest(w http.ResponseWriter, r *http.Request) {
	u.serveRaw(w, r, func(rec *record) ([]byte, string) { return rec.ReqBody, "application/json" })
}

func (u *uiServer) rawSent(w http.ResponseWriter, r *http.Request) {
	u.serveRaw(w, r, func(rec *record) ([]byte, string) { return rec.OpenAIBody, "application/json" })
}

func (u *uiServer) rawResponse(w http.ResponseWriter, r *http.Request) {
	u.serveRaw(w, r, func(rec *record) ([]byte, string) { return rec.RespBytes, "text/plain; charset=utf-8" })
}

func (u *uiServer) serveRaw(w http.ResponseWriter, r *http.Request, pick func(*record) ([]byte, string)) {
	rec := u.st.get(r.PathValue("id"))
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	body, ct := pick(rec)
	w.Header().Set("Content-Type", ct)
	w.Write(body)
}

// ---- settings ----

type settingsView struct {
	C            config
	EnvPath      string
	ProvPath     string
	Flash        string
	Err          string
	Seen         []routePreview
	Suggest      []string
	ModelChanged bool
	Models       []candidate           // configured local models in failover order
	Providers    []providerRow         // configured providers
	Pick         *providerRow          // the provider open in the picker, if any
	Info         map[string]probeModel // by provider/model, from the last probes
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

type routePreview struct {
	Model string
	Route string
	Count int
	Last  time.Time
}

func (u *uiServer) settingsView() settingsView {
	c := u.cs.get()
	v := settingsView{C: c, EnvPath: u.cs.envPath, ProvPath: u.cs.provPath}
	v.Models = u.ranked(c)
	v.Info = map[string]probeModel{}
	for _, p := range c.local.Providers {
		row := u.providerRow(c, p, false, modelFilter{})
		v.Providers = append(v.Providers, row)
		for _, m := range row.Probe.Info {
			v.Info[p.Name+"/"+m.ID] = m
		}
	}
	counts := map[string]*routePreview{}
	for _, rec := range u.st.list() {
		if rec.Model == "" {
			continue
		}
		p, ok := counts[rec.Model]
		if !ok {
			p = &routePreview{Model: rec.Model, Last: rec.Start}
			counts[rec.Model] = p
		}
		p.Count++
	}
	for _, p := range counts {
		p.Route = "cloud"
		if c.isLocal(p.Model) {
			p.Route = "local"
		}
		v.Seen = append(v.Seen, *p)
	}
	sort.Slice(v.Seen, func(i, j int) bool { return v.Seen[i].Model < v.Seen[j].Model })
	have := map[string]bool{}
	for _, e := range c.cloudOnly {
		have[e] = true
	}
	for _, s := range []string{"claude-opus-5", "claude-fable-5", "claude-sonnet-5", "claude-haiku-4-5"} {
		if !have[s] {
			v.Suggest = append(v.Suggest, s)
		}
	}
	return v
}

func (u *uiServer) settings(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "" {
		u.render(w, "layout", pageView{})
		return
	}
	u.render(w, "settings", u.settingsView())
}

func (u *uiServer) settingsSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	in := settingsInput{
		MaxInputChars: r.FormValue("max_input_chars"),
		CloudOnly:     r.Form["cloud_only"],
		Failover:      r.FormValue("failover"),
		FirstByte:     r.FormValue("first_byte"),
	}
	if extra := strings.TrimSpace(r.FormValue("cloud_only_new")); extra != "" {
		in.CloudOnly = append(in.CloudOnly, splitList(extra)...)
	}
	err := u.cs.apply(in, true)
	v := u.settingsView()
	if err != nil {
		v.Err = err.Error()
		// Show what the user typed, not what is saved, so nothing is lost.
		v.C.cloudOnly = in.CloudOnly
		v.C.maxInputChars = atoiOr(in.MaxInputChars, v.C.maxInputChars)
	} else {
		v.Flash = "Сохранено и применено. Файл: " + u.cs.envPath
		log.Printf("ui: settings applied: budget=%d cloud-only=%s failover=%v first-byte=%s",
			v.C.maxInputChars, strings.Join(v.C.cloudOnly, ","), v.C.failover, v.C.firstByte)
	}
	u.render(w, "settings", v)
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(c rune) bool { return c == ',' || c == '\n' || c == ' ' })
}

// settingsRoute flips one model family between cloud and local without the
// rest of the form: the quick toggle in the routing table.
func (u *uiServer) settingsRoute(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	entry := strings.TrimSpace(r.FormValue("entry"))
	c := u.cs.get()
	var cloud []string
	found := false
	for _, e := range c.cloudOnly {
		if e == entry {
			found = true
			continue
		}
		cloud = append(cloud, e)
	}
	if !found && entry != "" {
		cloud = append(cloud, entry)
	}
	in := inputFromConfig(c)
	in.CloudOnly = cloud
	v := settingsView{}
	if err := u.cs.apply(in, true); err != nil {
		v = u.settingsView()
		v.Err = err.Error()
	} else {
		v = u.settingsView()
		v.Flash = "Маршрут изменён: " + entry
		log.Printf("ui: cloud-only now %s", strings.Join(v.C.cloudOnly, ","))
	}
	u.render(w, "settings", v)
}

// settingsProbe re-probes one provider and re-renders its pane.
func (u *uiServer) settingsProbe(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	c := u.cs.get()
	p, ok := c.local.provider(strings.TrimSpace(r.FormValue("provider")))
	if !ok {
		u.render(w, "settings", u.settingsView())
		return
	}
	u.render(w, "provider", u.providerRow(c, p, true, modelFilter{}))
}

// settingsProvider renders the pane for the provider chosen in the picker:
// its probe, the models it offers that are not configured yet, and its form.
// An unknown or empty name is the form for a new provider.
func (u *uiServer) settingsProvider(w http.ResponseWriter, r *http.Request) {
	c := u.cs.get()
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if p, ok := c.local.provider(name); ok {
		u.render(w, "provider", u.providerRow(c, p, r.URL.Query().Get("filter") == "", filterFrom(r.URL.Query())))
		return
	}
	u.render(w, "provider", providerRow{Fresh: true, P: provider{BaseURL: "http://127.0.0.1:1234/v1"}})
}

// settingsProviders adds, edits or removes a provider. Removing one drops its
// models too; the ids stay local for running sessions via extraLocal.
func (u *uiServer) settingsProviders(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	c := u.cs.get()
	l := c.local.clone()
	name := strings.TrimSpace(r.FormValue("name"))
	orig := strings.TrimSpace(r.FormValue("orig")) // name before an edit
	var err error
	flash := ""
	switch op := r.FormValue("op"); op {
	case "add":
		if _, dup := l.provider(name); dup {
			err = fmt.Errorf("провайдер %q уже есть", name)
			break
		}
		l.Providers = append(l.Providers, provider{Name: name, BaseURL: r.FormValue("base_url"), APIKey: r.FormValue("api_key")})
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
		if _, dup := l.provider(name); dup && name != orig {
			err = fmt.Errorf("провайдер %q уже есть", name)
			break
		}
		p := &l.Providers[idx]
		p.Name = name
		p.BaseURL = r.FormValue("base_url")
		switch {
		case r.FormValue("clear_key") == "1":
			p.APIKey = ""
		case r.FormValue("api_key") != "":
			p.APIKey = r.FormValue("api_key")
		}
		for i := range l.Models {
			if l.Models[i].Provider == orig {
				l.Models[i].Provider = name
			}
		}
		if strings.HasPrefix(l.Preferred, orig+"/") {
			l.Preferred = name + strings.TrimPrefix(l.Preferred, orig)
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
		flash = "Провайдер удалён: " + name
	default:
		err = fmt.Errorf("unknown op %q", op)
	}
	if err == nil {
		err = u.cs.applyLocal(l, true)
	}
	v := u.settingsView()
	if err != nil {
		v.Err = err.Error()
	} else {
		n := u.cs.get()
		v.Flash = flash
		v.ModelChanged = c.local.Preferred != n.local.Preferred
		log.Printf("ui: providers: %s", n.local.summary())
		if p, ok := n.local.provider(name); ok {
			row := u.providerRow(n, p, true, modelFilter{})
			v.Pick = &row
		}
	}
	u.render(w, "settings", v)
}

// providerRow probes the provider and lists which of its models can still be added.
func (u *uiServer) providerRow(c config, p provider, force bool, f modelFilter) providerRow {
	row := providerRow{P: p, KeySet: p.APIKey != "", Probe: u.probeProvider(p, force), Filter: f}
	for _, m := range c.local.Models {
		if m.Provider == p.Name {
			row.NModels++
		}
	}
	row.Facets = append([]facet(nil), row.Probe.Facets...)
	for i := range row.Facets {
		row.Facets[i].Picked = f.Pick[row.Facets[i].Key]
	}
	for _, m := range row.Probe.Info {
		if c.local.hasModel(p.Name + "/" + m.ID) {
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
func (u *uiServer) probeProvider(p provider, force bool) probeResult {
	u.probeMu.Lock()
	defer u.probeMu.Unlock()
	if u.probe == nil {
		u.probe = map[string]probeResult{}
	}
	if old, ok := u.probe[p.Name]; ok && !force && old.Base == p.BaseURL && old.Key == p.APIKey && time.Since(old.At) < 5*time.Second {
		return old
	}
	res := probeResult{At: time.Now(), Base: p.BaseURL, Key: p.APIKey}
	defer func() { u.probe[p.Name] = res }()
	req, err := http.NewRequest("GET", p.BaseURL+"/models", nil)
	if err != nil {
		res.Msg = err.Error()
		return res
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		res.Msg = "недоступен: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		res.Msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return res
	}
	res.Info = parseModels(body)
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
	if len(preview) > 140 {
		preview = preview[:140] + "…"
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
	c.failover = true
	return u.hl.pick(c)
}

// settingsModels handles the one-click actions on the local models table.
// Models are addressed by key (provider/model); add takes provider + model.
func (u *uiServer) settingsModels(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	c := u.cs.get()
	l := c.local.clone()
	key := strings.TrimSpace(r.FormValue("key"))
	pname := strings.TrimSpace(r.FormValue("provider"))
	var err error
	op := r.FormValue("op")
	local := true // op changes providers.json rather than env or ratings
	switch op {
	case "add":
		for _, id := range splitList(r.FormValue("model")) {
			l.Models = append(l.Models, localModel{Provider: pname, Model: id})
		}
	case "remove":
		var keep []localModel
		for _, m := range l.Models {
			if m.Key() != key {
				keep = append(keep, m)
			}
		}
		l.Models = keep
	case "prefer":
		if !l.hasModel(key) {
			err = fmt.Errorf("модель %q не настроена", key)
		}
		l.Preferred = key
	case "failover":
		local = false
		in := inputFromConfig(c)
		in.Failover = "0"
		if r.FormValue("on") == "1" {
			in.Failover = "1"
		}
		err = u.cs.apply(in, true)
	case "reset":
		local = false
		u.hl.reset(key) // "" resets all
	default:
		local = false
		err = fmt.Errorf("unknown op %q", op)
	}
	if err == nil && local {
		err = u.cs.applyLocal(l, true)
	}
	v := u.settingsView()
	if err != nil {
		v.Err = err.Error()
	} else {
		n := u.cs.get()
		log.Printf("ui: local models: %s failover=%v", n.local.summary(), n.failover)
		v.ModelChanged = c.local.Preferred != n.local.Preferred
		if p, ok := n.local.provider(pname); ok && op == "add" {
			row := u.providerRow(n, p, false, modelFilter{})
			v.Pick = &row
		}
	}
	u.render(w, "settings", v)
}
