package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"localrouter/internal/history"
	webui "localrouter/internal/ui"
	"localrouter/internal/uisession"
)

// uiBackend is the seam between private runtime/config types and the public,
// explicitly credential-free read model. Reads never probe a remote provider.
type uiBackend struct{ u *uiServer }

func (b uiBackend) Writable() bool { return b.u.life == nil || b.u.life.writesSharedState() }

func uiRoutes(in map[string]map[string]modelRoute) map[string]map[string]webui.Route {
	out := map[string]map[string]webui.Route{}
	for name, rules := range in {
		row := map[string]webui.Route{}
		for e, r := range rules {
			row[e] = webui.Route{Mode: r.Mode, Pool: r.Pool, Model: r.Model, Effort: r.Effort}
		}
		out[name] = row
	}
	return out
}
func uiProfile(name string, p routingProfile) webui.Profile {
	out := webui.Profile{Name: name, DefaultPool: p.DefaultPool, FamilyRoutes: uiRoutes(p.FamilyRoutes), Routes: uiRoutes(p.Routes), ModelPools: map[string][]webui.Target{}}
	for name, targets := range p.ModelPools {
		v := []webui.Target{}
		for _, t := range targets {
			v = append(v, webui.Target{Model: t.Model, Effort: t.Effort, EffortMap: t.clone().EffortMap})
		}
		out.ModelPools[name] = v
	}
	return out
}
func safeBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func (b uiBackend) State(ctx context.Context) webui.State {
	u := b.u
	c := u.cs.get()
	now := time.Now()
	records := u.st.List()
	usage := u.connectionMetrics.view(records, now, c.local.Providers)
	out := webui.State{Now: now, Started: u.started, Lifecycle: "active", ActiveProfile: c.local.ActiveProfile, Profiles: []webui.Profile{}, Connections: []webui.Connection{}, Models: []webui.Model{}, Families: []webui.RouteRow{}, Routes: []webui.RouteRow{}, Pools: []webui.Pool{}, Efforts: append([]string{}, claudeEfforts...), Sessions: []webui.Session{}, ReloadErrors: []string{}}
	out.DefaultPool = c.local.DefaultPool
	if u.life != nil {
		out.Lifecycle = string(u.life.mode())
	}
	for name, p := range c.local.Profiles {
		if name == c.local.ActiveProfile {
			p = c.local.routing()
		}
		out.Profiles = append(out.Profiles, uiProfile(name, p))
	}
	sort.Slice(out.Profiles, func(i, j int) bool { return out.Profiles[i].Name < out.Profiles[j].Name })
	cp := u.claudeProxyView()
	out.Interception = webui.Interception{Enabled: cp.Enabled, CanRestore: cp.CanRestore, Error: u.publicUIMessage(cp.Error)}
	for _, failure := range u.cs.reloadFailures() {
		out.ReloadErrors = append(out.ReloadErrors, u.publicUIMessage(failure.Err))
	}
	observed := u.limits.View(now)
	cloud := webui.Connection{Name: "anthropic", DisplayName: "Anthropic", Type: "anthropic", Connected: true, Models: append([]string{}, c.local.Catalog.Anthropic...), Limits: []webui.Limit{}, Updated: observed.ObservedAt}
	for _, w := range observed.Windows {
		v := webui.Limit{ID: w.Name, Label: w.Name, Known: w.RemainingPercent != nil, Reset: w.ResetAt, Blocked: w.Status == "rejected"}
		if w.RemainingPercent != nil {
			v.Remaining = *w.RemainingPercent
		}
		cloud.Limits = append(cloud.Limits, v)
	}
	out.Connections = append(out.Connections, cloud)
	infos := map[string]probeModel{}
	u.probeMu.Lock()
	probes := make(map[string]probeResult, len(u.probe))
	for name, p := range u.probe {
		probes[name] = p
	}
	u.probeMu.Unlock()
	for _, p := range c.local.Providers {
		v := webui.Connection{Name: p.Name, DisplayName: p.DisplayName, Type: p.Type, BaseURL: safeBaseURL(p.BaseURL), KeySet: p.APIKey != "", Models: []string{}, Limits: []webui.Limit{}}
		if v.DisplayName == "" {
			v.DisplayName = p.Name
		}
		if v.Type == "" {
			v.Type = "openai"
		}
		catalog := c.local.Catalog.Providers[p.Name]
		v.Updated = catalog.UpdatedAt
		v.Error = catalog.Error
		for _, m := range catalog.Models {
			v.Models = append(v.Models, m.ID)
			infos[p.Name+"/"+m.ID] = probeModel{ID: m.ID, Efforts: m.Efforts}
		}
		if probe, ok := probes[p.Name]; ok && probe.Base == p.BaseURL && probe.Key == p.APIKey && probe.AuthID == p.AuthID {
			v.Connected = probe.OK
			if !probe.OK {
				v.Error = probe.Msg
			}
			v.Models = append([]string{}, probe.Models...)
			for _, m := range probe.Info {
				infos[p.Name+"/"+m.ID] = m
			}
		}
		if p.Type == "codex" {
			u.codexUIState(p, &v)
		}
		v.Error = u.publicUIMessage(v.Error)
		out.Connections = append(out.Connections, v)
	}
	for i := range out.Connections {
		out.Connections[i].Usage = usage.connections[out.Connections[i].Name]
		out.Connections[i].Usage.Since = now.Add(-24 * time.Hour)
	}
	for _, m := range c.local.Models {
		out.Models = append(out.Models, webui.Model{Key: m.Key(), Provider: m.Provider, Model: m.Model, Efforts: append([]string{}, modelEffortOptions(c.local, m.Key(), infos)...)})
	}
	families := map[string]bool{"opus": true, "sonnet": true, "haiku": true, "fable": true}
	models := map[string]bool{}
	for f := range c.local.FamilyRoutes {
		families[f] = true
	}
	for m := range c.local.Routes {
		models[m] = true
	}
	for _, m := range c.local.Catalog.Anthropic {
		models[m] = true
	}
	if len(models) == 0 {
		for _, m := range anthropicModels {
			models[m] = true
		}
	}
	row := func(name string, family bool) webui.RouteRow {
		r := webui.RouteRow{Model: name, Label: name, Choices: []webui.Choice{}}
		rules := c.local.Routes[name]
		if family {
			rules = c.local.FamilyRoutes[name]
			r.Label = strings.ToUpper(name[:1]) + name[1:]
		}
		for _, e := range claudeEfforts {
			route, ok := rules[e]
			dest := "disabled"
			if ok {
				dest = routeDestination(route)
			} else if family && len(rules) == 0 && c.local.DefaultPool != "" {
				dest = "inherit"
			} else if !family && claudeFamily(name) != "" {
				dest = "inherit"
			}
			inherited := routeDestination(c.local.FamilyRoutes[claudeFamily(name)][e])
			if inherited == "" {
				inherited = "disabled"
			}
			if c.local.defaultPoolFor(name) != "" {
				inherited = "pool:" + c.local.DefaultPool
			}
			r.Choices = append(r.Choices, webui.Choice{Effort: e, Destination: dest, Inherited: inherited, Configured: ok})
		}
		return r
	}
	for name := range families {
		if name != "" {
			out.Families = append(out.Families, row(name, true))
		}
	}
	sort.Slice(out.Families, func(i, j int) bool { return out.Families[i].Model < out.Families[j].Model })
	for name := range models {
		out.Routes = append(out.Routes, row(name, false))
	}
	sort.Slice(out.Routes, func(i, j int) bool { return out.Routes[i].Model < out.Routes[j].Model })
	for name, targets := range c.local.ModelPools {
		settings := c.poolSettings(name)
		p := webui.Pool{Name: name, Type: settings.Type, MaxInputChars: settings.MaxInputChars, Failover: settings.Failover, FirstByte: settings.FirstByteSec, ProbeEvery: settings.ProbeSec, Members: []webui.Member{}}
		for _, t := range targets {
			mapping := map[string]string{}
			for source, effort := range t.EffortMap {
				mapping[source] = effort
			}
			p.Members = append(p.Members, webui.Member{Model: t.Model, Effort: t.Effort, EffortMap: mapping, Efforts: append([]string{}, modelEffortOptions(c.local, t.Model, infos)...), Cooling: u.hl.snapshot(t.Model).Cooling()})
		}
		out.Pools = append(out.Pools, p)
	}
	sort.Slice(out.Pools, func(i, j int) bool { return out.Pools[i].Name < out.Pools[j].Name })
	sessions := map[string]*webui.Session{}
	routeIndexes := map[string]map[[5]string]int{}
	sessionBodies := map[string][][]byte{}
	for _, rec := range records {
		out.Summary.Total++
		if !rec.Done() {
			out.Summary.Pending++
		}
		if rec.Failed() && rec.Start.After(now.Add(-5*time.Minute)) {
			out.Summary.Errors5m++
		}
		if (rec.UnrecognizedReason != "" && rec.FallbackPool == "") || (rec.Path != "" && rec.Path != "/v1/messages") {
			continue
		}
		model := rec.Served
		if model == "" {
			model = rec.Model
		}
		var request struct {
			OutputConfig struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
		}
		_ = json.Unmarshal(rec.ReqBody, &request)
		connection := uisession.Connection(rec.Route, rec.Served)
		s := sessions[rec.Session]
		if s == nil {
			item := b.requestDTO(rec)
			s = &webui.Session{ID: rec.Session, Model: model, RequestedModel: rec.Model, Preview: item.Preview, Connection: connection, Effort: request.OutputConfig.Effort, Route: rec.Route, LastAt: rec.Start, Routes: []webui.SessionRoute{}}
			sessions[rec.Session] = s
			routeIndexes[rec.Session] = map[[5]string]int{}
		}
		key := [5]string{rec.Model, model, connection, request.OutputConfig.Effort, rec.Route}
		index, exists := routeIndexes[rec.Session][key]
		if !exists {
			index = len(s.Routes)
			routeIndexes[rec.Session][key] = index
			s.Routes = append(s.Routes, webui.SessionRoute{RequestedModel: rec.Model, Model: model, Connection: connection, Effort: request.OutputConfig.Effort, Route: rec.Route})
		}
		s.Routes[index].Requests++
		if !rec.Done() {
			s.Routes[index].Pending++
		}
		sessionBodies[rec.Session] = append(sessionBodies[rec.Session], rec.ReqBody)
		s.Requests++
		if !rec.Done() {
			s.Pending++
		}
		if s.Error == "" && rec.Failed() {
			s.Error = u.publicUIMessage(requestError(rec))
		}
	}
	for _, s := range sessions {
		s.Usage = usage.sessions[s.ID]
		s.Usage.Since = now.Add(-24 * time.Hour)
		out.Sessions = append(out.Sessions, *s)
	}
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].LastAt.After(out.Sessions[j].LastAt) })
	if len(out.Sessions) > 100 {
		out.Sessions = out.Sessions[:100]
	}
	ids := make([]string, 0, len(out.Sessions))
	for _, session := range out.Sessions {
		ids = append(ids, session.ID)
	}
	root := ""
	if u.claudeProxy != nil && u.claudeProxy.path != "" {
		root = filepath.Join(filepath.Dir(u.claudeProxy.path), "projects")
	}
	names := u.sessionNames.Snapshot(root, ids)
	promptTitles := uisession.PromptTitles(u.cs.privacyRuntime())
	for i := range out.Sessions {
		session := &out.Sessions[i]
		identity := names[session.ID]
		session.Title, session.Project, session.Branch = identity.Title, identity.Project, identity.Branch
		if session.Title == "" && session.ID != "" && promptTitles {
			bodies := sessionBodies[session.ID]
			for j := len(bodies) - 1; j >= 0; j-- {
				if session.Title = uisession.FirstPrompt(bodies[j]); session.Title != "" {
					break
				}
			}
		}
	}
	return out
}
func requestError(r *history.Record) string {
	if r.Resp != nil && r.Resp.Error != "" {
		return r.Resp.Error
	}
	if r.Failed() {
		return fmt.Sprintf("Сервис не выполнил запрос (HTTP %d)", r.Status)
	}
	return ""
}
func (b uiBackend) requestDTO(r *history.Record) webui.Request {
	_, _, preview := quickSummary(r.ReqBody)
	method, path := r.Method, r.Path
	if method == "" {
		method = http.MethodPost
	}
	if path == "" {
		path = "/v1/messages"
	}
	reason := b.u.publicUIMessage(r.UnrecognizedReason)
	if reason == "" && r.Route == "disabled" {
		reason = b.u.publicUIMessage(requestError(r))
		if reason == "" {
			reason = "Маршрут не настроен"
		}
	}
	if preview == "" && reason != "" {
		preview = reason
	}
	connection := uisession.Connection(r.Route, r.Served)
	return webui.Request{ID: r.ID, Method: method, Path: path, Unrecognized: reason, FallbackPool: r.FallbackPool, Session: r.Session, Start: r.Start, End: r.End, Model: r.Model, Served: r.Served, Connection: connection, Route: r.Route, Status: r.Status, Pending: !r.Done(), Failed: r.Failed(), DurationMs: r.Duration().Milliseconds(), Preview: preview, Error: b.u.publicUIMessage(requestError(r))}
}
func (b uiBackend) Requests(q url.Values) webui.RequestList {
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 50
	}
	limit = max(1, min(limit, 200))
	offset, _ := strconv.Atoi(q.Get("offset"))
	offset = max(0, offset)
	out := webui.RequestList{Items: []webui.Request{}, Limit: limit, Offset: offset}
	records := b.u.st.List()
	if session := q.Get("session"); session != "" {
		now := time.Now()
		usage := b.u.connectionMetrics.view(records, now, b.u.cs.get().local.Providers).sessions[session]
		usage.Since = now.Add(-24 * time.Hour)
		out.SessionUsage = &usage
	}
	needle := strings.ToLower(q.Get("q"))
	for _, r := range records {
		if q.Get("session") != "" && r.Session != q.Get("session") {
			continue
		}
		if q.Get("model") != "" && !strings.Contains(strings.ToLower(r.Model+" "+r.Served), strings.ToLower(q.Get("model"))) {
			continue
		}
		if q.Get("connection") != "" && uisession.Connection(r.Route, r.Served) != q.Get("connection") {
			continue
		}
		if q.Get("errors") == "1" && !r.Failed() {
			continue
		}
		item := b.requestDTO(r)
		if q.Get("unrecognized") == "1" && item.Unrecognized == "" {
			continue
		}
		if needle != "" {
			response := ""
			if r.Resp != nil {
				response = r.Resp.Text()
			}
			if !strings.Contains(strings.ToLower(string(r.ReqBody)+" "+string(r.RespBytes)+" "+response+" "+item.Model+" "+item.Served+" "+item.Error+" "+item.Method+" "+item.Path+" "+item.Unrecognized), needle) {
				continue
			}
		}
		out.Total++
		if out.Total > offset && len(out.Items) < limit {
			out.Items = append(out.Items, item)
		}
	}
	return out
}
func boundedBody(body string) (string, bool) {
	r := []rune(body)
	if len(r) > 256<<10 {
		return string(r[:256<<10]) + "\n…", true
	}
	return body, false
}

// Read recorded payloads, never today's route configuration. nil means
// unavailable evidence; an empty effort means the field was actually absent.
func recordedEffort(body []byte, sent bool) *string {
	var payload struct {
		Model        string `json:"model"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Model == "" {
		return nil
	}
	effort := payload.OutputConfig.Effort
	if sent {
		effort = payload.ReasoningEffort
		if payload.Reasoning.Effort != "" {
			effort = payload.Reasoning.Effort
		}
	}
	return &effort
}

func (b uiBackend) Detail(id string) (webui.Detail, bool) {
	r := b.u.st.Get(id)
	if r == nil {
		return webui.Detail{}, false
	}
	d := webui.Detail{Request: b.requestDTO(r), Attempts: []webui.Attempt{}, Headers: map[string]string{}, Usage: map[string]int{}, Truncated: r.RespTruncated}
	d.RequestedEffort = recordedEffort(r.ReqBody, false)
	d.SentEffort = recordedEffort(r.OpenAIBody, true)
	d.CaptureTruncated = r.ReqTruncated || r.RespTruncated
	d.RequestNote = r.RequestNote
	var cut bool
	d.RequestBody, cut = boundedBody(string(r.ReqBody))
	d.Truncated = d.Truncated || cut
	d.Sent, cut = boundedBody(string(r.OpenAIBody))
	d.Truncated = d.Truncated || cut
	response := string(r.RespBytes)
	if r.Resp != nil {
		if d.Unrecognized == "" {
			response = r.Resp.Text()
		}
		for k, v := range r.Resp.Usage {
			d.Usage[k] = v
		}
	}
	if r.Failed() {
		response = b.u.publicUIMessage(response)
	}
	d.Response, cut = boundedBody(response)
	d.Truncated = d.Truncated || cut
	// Keep only non-secret protocol headers; arbitrary X-* headers can carry keys.
	for k, v := range r.Headers {
		switch strings.ToLower(k) {
		case "content-type", "content-encoding", "content-length", "anthropic-version", "anthropic-beta", "user-agent":
			d.Headers[k] = v
		}
	}
	for _, a := range r.Attempts {
		d.Attempts = append(d.Attempts, webui.Attempt{Model: a.Model, Error: b.u.publicUIMessage(a.Err), DurationMs: a.Dur.Milliseconds()})
	}
	return d, true
}

// actionResponse lets established mutation handlers report typed results without
// rendering HTML or building the legacy settings read model after each save.
type actionResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	result webui.Result
	err    error
}

func (w *actionResponse) Header() http.Header                { return w.header }
func (w *actionResponse) WriteHeader(code int)               { w.status = code }
func (w *actionResponse) Write(p []byte) (int, error)        { return w.body.Write(p) }
func (w *actionResponse) complete(err error, message string) { w.err = err; w.result.Message = message }
func (b uiBackend) Action(ctx context.Context, a webui.Action) (result webui.Result, actionErr error) {
	u := b.u
	defer func() {
		if actionErr != nil {
			actionErr = fmt.Errorf("%s", u.publicUIMessage(actionErr.Error()))
		}
	}()
	handlers := map[string]http.HandlerFunc{"route.save": u.settingsRoute, "profile.activate": u.profileActivate, "profile.create": u.profileCreate, "profile.delete": u.profileDelete, "pool.edit": u.settingsPools, "pool.settings": u.settingsPoolSave, "connection.edit": u.settingsProviders, "model.edit": u.settingsModels, "codex.login": u.settingsCodexLogin, "codex.import": u.settingsCodexImport, "codex.usage": u.settingsCodexUsage, "interception.set": u.settingsClaudeProxy, "catalog.refresh": u.settingsRefreshModels, "connection.probe": u.settingsProbe, "requests.clear": u.clear}
	fields := map[string]string{"route.save": "profile scope model all default low medium high xhigh max", "profile.activate": "name", "profile.create": "name mode", "profile.delete": "name", "pool.edit": "profile op name source key effort default low medium high xhigh max", "pool.settings": "profile name type max_input_chars failover first_byte probe_every", "connection.edit": "op name orig type base_url api_key clear_key display_name", "model.edit": "op provider model key on", "codex.login": "provider", "codex.import": "provider", "codex.usage": "provider", "interception.set": "op", "catalog.refresh": "", "connection.probe": "provider", "requests.clear": ""}
	handler, ok := handlers[a.Action]
	if !ok {
		return webui.Result{}, fmt.Errorf("Неизвестное действие")
	}
	for k := range a.Fields {
		if !webui.ValidField(k, fields[a.Action]) {
			return webui.Result{}, fmt.Errorf("Неизвестное поле: %s", k)
		}
	}
	if a.Action == "route.save" {
		if strings.TrimSpace(a.Fields["model"]) == "" || (a.Fields["scope"] != "family" && a.Fields["scope"] != "model") {
			return webui.Result{}, fmt.Errorf("Укажите модель и область маршрута")
		}
		if a.Fields["all"] == "" {
			for _, effort := range claudeEfforts {
				if a.Fields[effort] == "" {
					return webui.Result{}, fmt.Errorf("Не указано назначение для %s; маршруты не изменены", effort)
				}
			}
		}
	}
	if a.Action == "route.save" || a.Action == "pool.edit" || a.Action == "pool.settings" {
		if _, ok := a.Fields["profile"]; !ok || a.Fields["profile"] != u.cs.get().local.ActiveProfile {
			return webui.Result{}, fmt.Errorf("Активный профиль изменился; обновите страницу")
		}
	}
	if a.Action == "connection.edit" && a.Fields["op"] == "remove" {
		return u.removeUIModelOrConnection(a.Fields["name"], "")
	}
	if a.Action == "model.edit" && a.Fields["op"] == "remove" {
		return u.removeUIModelOrConnection("", a.Fields["key"])
	}
	if a.Action == "connection.probe" {
		if _, ok := u.cs.get().local.provider(a.Fields["provider"]); !ok {
			return webui.Result{}, fmt.Errorf("Подключение не найдено")
		}
	}
	form := url.Values{}
	for k, v := range a.Fields {
		form.Set(k, v)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Form = form
	req.PostForm = form
	w := &actionResponse{header: make(http.Header), status: 200, result: webui.Result{Message: "Изменения применены"}}
	handler(w, req)
	if w.err != nil {
		return webui.Result{}, w.err
	}
	if w.status >= 400 {
		return webui.Result{}, fmt.Errorf("%s", strings.TrimSpace(w.body.String()))
	}
	if w.status == http.StatusSeeOther {
		w.result.URL = w.header.Get("Location")
		w.result.Message = "Откройте страницу входа Codex"
	}
	return w.result, nil
}

// Transport errors may include an endpoint URL. A display error must not echo
// credentials from the configured URL or API key back into browser state.
func (u *uiServer) publicUIMessage(message string) string {
	if message == "" {
		return ""
	}
	for _, p := range u.cs.get().local.Providers {
		if p.BaseURL != "" {
			message = strings.ReplaceAll(message, p.BaseURL, safeBaseURL(p.BaseURL))
		}
		for _, secret := range []string{p.APIKey, p.AuthID} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[скрыто]")
			}
		}
		if parsed, err := url.Parse(p.BaseURL); err == nil {
			for _, values := range parsed.Query() {
				for _, secret := range values {
					if secret != "" {
						message = strings.ReplaceAll(message, secret, "[скрыто]")
					}
				}
			}
			if parsed.User != nil {
				if password, ok := parsed.User.Password(); ok && password != "" {
					message = strings.ReplaceAll(message, password, "[скрыто]")
				}
			}
		}
	}
	return message
}
func rejectReferencedModel(l localSetup, matches func(string) bool) error {
	profiles := map[string]routingProfile{}
	for name, p := range l.Profiles {
		profiles[name] = p
	}
	profiles[l.ActiveProfile] = l.routing()
	for name, p := range profiles {
		for pool, targets := range p.ModelPools {
			for _, t := range targets {
				if matches(t.Model) {
					return fmt.Errorf("Сначала удалите модель из пула %q профиля %q", pool, name)
				}
			}
		}
		for _, all := range []map[string]map[string]modelRoute{p.Routes, p.FamilyRoutes} {
			for model, rules := range all {
				for _, r := range rules {
					if r.Mode == "model" && matches(r.Model) {
						return fmt.Errorf("Сначала измените маршрут %q профиля %q", model, name)
					}
				}
			}
		}
	}
	return nil
}

// The reference check and write share the config lock. A parallel profile
// activation or manual reload cannot introduce references between them.
func (u *uiServer) removeUIModelOrConnection(name, key string) (webui.Result, error) {
	u.cs.mu.Lock()
	defer u.cs.mu.Unlock()
	l := u.cs.c.local.clone()
	matches := func(k string) bool { return k == key }
	if name != "" {
		matches = func(k string) bool { return strings.HasPrefix(k, name+"/") }
	}
	if err := rejectReferencedModel(l, matches); err != nil {
		return webui.Result{}, err
	}
	if name != "" {
		found := false
		providers := []provider{}
		for _, p := range l.Providers {
			if p.Name == name {
				found = true
			} else {
				providers = append(providers, p)
			}
		}
		if !found {
			return webui.Result{}, fmt.Errorf("Подключение не найдено")
		}
		l.Providers = providers
		delete(l.Catalog.Providers, name)
		delete(l.Catalog.CodexSeen, name)
	} else if key == "" || !l.hasModel(key) {
		return webui.Result{}, fmt.Errorf("Модель не найдена")
	}
	models := []localModel{}
	for _, m := range l.Models {
		if !matches(m.Key()) {
			models = append(models, m)
		}
	}
	l.Models = models
	if err := u.cs.applyLocalLocked(l, true); err != nil {
		return webui.Result{}, err
	}
	return webui.Result{Message: "Удалено; маршруты и пулы сохранены"}, nil
}

// codexUIState never waits on a token refresh. TryLock on both locks avoids
// inverting the cache -> auth lock order used by the network refresh worker.
func (u *uiServer) codexUIState(p provider, v *webui.Connection) {
	v.Updated = time.Time{}
	// Model discovery may have failed before login. Its cached error does not
	// describe the current credential, OAuth flow or subscription usage.
	v.Error = ""
	u.oauthMu.Lock()
	if u.oauthTarget.Name == p.Name && u.oauthTarget.AuthID == p.AuthID {
		v.Pending = u.oauthStatus == "pending"
		if u.oauthError != "" {
			v.Error = u.oauthError
		}
	}
	u.oauthMu.Unlock()
	auth, err := codexStoreFor(p)
	if err != nil {
		v.Error = err.Error()
		return
	}
	if !auth.mu.TryLock() {
		v.Refreshing = true
		v.Pending = true
		return
	}
	defer auth.mu.Unlock()
	credential := auth.credential
	if !auth.loaded {
		credential, err = readCodexCredential(auth.path)
	}
	v.Connected = err == nil
	if auth.authProblem != "" {
		v.Error = auth.authProblem
	}
	cache := u.usageCache(p)
	if !cache.mu.TryLock() {
		v.Refreshing = true
		return
	}
	defer cache.mu.Unlock()
	account := accountFromCredential(credential)
	if !v.Connected || account.key() != cache.key {
		cache.key = ""
		cache.view = codexUsageView{Connected: v.Connected}
	}
	usage := cache.view
	v.Updated = usage.Updated
	v.ResetsKnown, v.Resets = usage.ResetsKnown, usage.Resets
	if usage.Error != "" {
		v.Error = usage.Error
	}
	for _, lim := range usage.Limits {
		v.Limits = append(v.Limits, webui.Limit{ID: lim.ID, Label: lim.Name, Known: lim.Known, Remaining: lim.Remaining, Reset: lim.Reset, Blocked: lim.Blocked})
	}
}
