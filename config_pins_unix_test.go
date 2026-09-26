//go:build unix

package main

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	conf "localrouter/internal/config"
)

// A write that fails returns the writer's error and changes neither memory
// nor any file, and the next tick reloads nothing. A read-only home makes
// every atomic write fail at its temp file.
func TestConfigPinFailedWritesChangeNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	p := configPinStart(t, "home", false, nil)
	profiles := p.path + ".profiles"
	for _, dir := range []string{p.dir, profiles} {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range []string{p.dir, profiles} {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Error(err)
			}
		}
	})
	envTemp := regexp.MustCompile(`/\.env\.[0-9]+:`)
	rows := []struct {
		name string
		op   func() error
		want string
	}{
		{"replace", func() error {
			l := p.cs.Get().Local.Clone()
			l.Models = append(l.Models, localModel{Provider: "lab", Model: "qwen-coder-next"})
			return p.cs.Update(conf.Replace(l, ""))
		}, "запись " + p.path + ": open " + p.path + ".tmp: permission denied"},
		{"settings", func() error {
			return p.cs.UpdateSettings(func(in *conf.SettingsInput) error {
				in.MaxInputChars = "150000"
				return nil
			})
		}, "запись " + p.dir + "/env: open " + p.dir + "/.env.*: permission denied"},
		{"pool settings", func() error {
			return p.cs.SavePoolSettings("work", poolSettings{Type: conf.PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15})
		}, "запись " + p.path + ": open " + profiles + "/default.json.tmp: permission denied"},
		{"create profile", func() error { return p.cs.CreateProfile("night", true) }, "запись " + p.path + ": open " + profiles + "/night.json.tmp: permission denied"},
		{"activate profile", func() error { return p.cs.ActivateProfile("cloud") }, "open " + p.path + ".active-profile.tmp: permission denied"},
		{"delete profile", func() error { return p.cs.DeleteProfile("cloud") }, "remove " + profiles + "/cloud.json: permission denied"},
	}
	p.seat()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			mem, files := p.get(), p.files(t)
			err := row.op()
			if err == nil {
				t.Fatalf("no error, want %q", row.want)
			}
			if got := envTemp.ReplaceAllString(err.Error(), "/.env.*:"); got != row.want {
				t.Errorf("error %q, want %q", got, row.want)
			}
			if got := p.get(); !reflect.DeepEqual(got, mem) {
				t.Errorf("config changed:\n got %+v\nwant %+v", got, mem)
			}
			if got := p.files(t); !reflect.DeepEqual(got, files) {
				t.Errorf("files changed:\n got %v\nwant %v", got, files)
			}
			if strings.Contains(p.logs.String(), "reload") {
				t.Errorf("a failed write logged a reload:\n%s", p.logs.String())
			}
		})
	}
	if !p.seated() {
		t.Error("a failed write cleared the sessions")
	}
	p.pollQuiet(t)
}
