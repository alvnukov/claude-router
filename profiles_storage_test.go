package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	conf "localrouter/internal/config"
	"localrouter/internal/history"
)

func TestStaleRoutingFormRejectedAfterActivation(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	if err := cs.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"profile": {"default"}, "scope": {"family"}, "model": {"opus"}, "high": {"anthropic"}}
	req := httptest.NewRequest("POST", "/settings/route", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if cs.Get().Local.FamilyRoutes["opus"]["high"].Mode == "anthropic" {
		t.Fatal("stale routing form edited new active profile")
	}
}

func TestStalePoolSettingsFormRejectedAfterActivation(t *testing.T) {
	cs, h, _ := profileFixture(t)
	l := cs.Get().Local.Clone()
	l.ModelPools = map[string][]poolTarget{"work": {{Model: "p/a"}}}
	l.PoolSettings = map[string]poolSettings{"work": {FirstByteSec: 2}}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("cloud", true); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	if err := cs.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"profile": {"default"}, "name": {"work"}, "first_byte": {"1"}, "probe_every": {"0"}, "max_input_chars": {"0"}}
	req := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if cs.Get().Local.PoolSettings["work"].FirstByteSec != 2 {
		t.Fatal("stale pool settings changed new active profile")
	}
}
