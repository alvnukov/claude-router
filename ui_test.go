package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Templates are parsed at startup only; a stray {{end}} must fail here,
// not when launchd restarts the router.
func TestTemplatesParse(t *testing.T) {
	if _, err := uiTemplates(); err != nil {
		t.Fatal(err)
	}
}

// testUI builds a UI server over an in-memory history with one cloud and one
// local record per session, a provider that answers /models, and a finished
// response on every record so all detail views have something to show.
func testUI(t *testing.T, sessions ...string) (*uiServer, http.Handler) {
	t.Helper()
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	}))
	t.Cleanup(prov.Close)
	cs := &configStore{c: config{local: oneProvider(prov.URL, "m1"), failover: true, firstByte: 45 * time.Second}}
	st := newStore(100, "")
	for _, sid := range sessions {
		meta := fmt.Sprintf(`{"metadata":{"user_id":"{\"session_id\":\"%s\"}"},"messages":[{"role":"user","content":"hello"}]}`, sid)
		for _, route := range []string{"cloud", "local"} {
			r := &record{Start: time.Now(), Model: "claude-x", Route: route, ReqBody: []byte(meta), Session: sid}
			if route == "local" {
				r.Served = "p/m1"
				r.OpenAIBody = []byte(`{"model":"m1","messages":[]}`)
			}
			st.add(r)
			rw := newRecorder(httptest.NewRecorder(), 1<<20)
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(200)
			rw.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn"}`))
			st.finish(r.ID, rw, nil)
		}
	}
	u := newUIServer(st, cs, newHealth(""))
	return u, u.handler()
}

func get(t *testing.T, h http.Handler, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%s %s: %d %s", method, target, w.Code, firstLine(w.Body.String()))
	}
	if b := w.Body.String(); strings.Contains(b, "<no value>") {
		t.Fatalf("%s %s: <no value> in output", method, target)
	}
	return w
}

// Every page and partial must execute on real data: a bad field reference
// only fails at execution time, which parsing alone never catches.
func TestUIPagesRender(t *testing.T) {
	u, h := testUI(t, "s1", "s2")
	get(t, h, "GET", "/", nil)
	get(t, h, "GET", "/status", nil)
	get(t, h, "GET", "/requests", nil)
	get(t, h, "GET", "/requests?route=local&q=hi&model=claude", nil)
	for _, rec := range u.st.list() {
		for _, view := range []string{"structure", "sent", "response", "raw"} {
			get(t, h, "GET", "/requests/"+rec.ID+"?view="+view+"&q=hi", nil)
		}
	}
	get(t, h, "GET", "/settings", nil)
	get(t, h, "GET", "/settings/provider?name=p", nil)
	get(t, h, "GET", "/settings/provider?name=", nil)
	get(t, h, "POST", "/settings/probe", url.Values{"provider": {"p"}})
	get(t, h, "POST", "/settings/models", url.Values{"op": {"reset"}})
	if !strings.Contains(get(t, h, "GET", "/requests", nil).Body.String(), "→ p/m1") {
		t.Fatal("list does not show provider/model next to the requested model")
	}
}

func TestListCountsAndSessions(t *testing.T) {
	_, h := testUI(t, "s1", "s2", "s3") // 6 records in 3 sessions, newest first
	body := get(t, h, "GET", "/requests?limit=3", nil).Body.String()
	if !strings.Contains(body, "показано 3 из 6") || !strings.Contains(body, "сессий 2") {
		t.Fatalf("limit=3: %s", firstLine(body))
	}
	if n := strings.Count(body, `class="item`); n != 3 {
		t.Fatalf("limit=3 rendered %d items", n)
	}
	body = get(t, h, "GET", "/requests?session=s2", nil).Body.String()
	if !strings.Contains(body, "показано 2 из 6") || !strings.Contains(body, "сессий 1") || strings.Count(body, `data-sid="s2"`) != 1 {
		t.Fatalf("session filter: %s", firstLine(body))
	}
	body = get(t, h, "GET", "/requests?route=cloud", nil).Body.String()
	if !strings.Contains(body, "показано 3 из 6") || strings.Contains(body, `class="item local`) {
		t.Fatalf("route filter: %s", firstLine(body))
	}
}
