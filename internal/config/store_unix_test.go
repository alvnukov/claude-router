//go:build unix

package config

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// A write that fails returns the writer's error and changes neither memory
// nor any file, and the next tick reloads nothing. A read-only home makes
// every atomic write fail at its temp file.
func TestFailedWritesChangeNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	h := startHome(t, "home", false, nil)
	profiles := h.path + ".profiles"
	for _, dir := range []string{h.dir, profiles} {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range []string{h.dir, profiles} {
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
			l := h.s.Get().Local.Clone()
			l.Models = append(l.Models, Model{Provider: "lab", Model: "qwen-coder-next"})
			return h.s.Update(Replace(l, ""))
		}, "запись настроек провайдеров: open " + h.path + ".tmp: permission denied"},
		{"settings", func() error {
			return h.s.UpdateSettings(func(in *SettingsInput) error {
				in.MaxInputChars = "150000"
				return nil
			})
		}, "запись " + h.dir + "/env: open " + h.dir + "/.env.*: permission denied"},
		{"pool settings", func() error {
			return h.s.SavePoolSettings("work", PoolSettings{Type: PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15})
		}, "запись настроек провайдеров: open " + profiles + "/default.json.tmp: permission denied"},
		{"create profile", func() error { return h.s.CreateProfile("night", true) }, "запись настроек провайдеров: open " + profiles + "/night.json.tmp: permission denied"},
		{"activate profile", func() error { return h.s.ActivateProfile("cloud") }, "open " + h.path + ".active-profile.tmp: permission denied"},
		{"delete profile", func() error { return h.s.DeleteProfile("cloud") }, "remove " + profiles + "/cloud.json: permission denied"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			mem, files := h.get(), h.files(t)
			err := row.op()
			if err == nil {
				t.Fatalf("no error, want %q", row.want)
			}
			if got := envTemp.ReplaceAllString(err.Error(), "/.env.*:"); got != row.want {
				t.Errorf("error %q, want %q", got, row.want)
			}
			if got := h.get(); !reflect.DeepEqual(got, mem) {
				t.Errorf("config changed:\n got %+v\nwant %+v", got, mem)
			}
			if got := h.files(t); !reflect.DeepEqual(got, files) {
				t.Errorf("files changed:\n got %v\nwant %v", got, files)
			}
			if strings.Contains(h.logs.String(), "reload") {
				t.Errorf("a failed write logged a reload:\n%s", h.logs.String())
			}
		})
	}
	if h.hooks != 0 {
		t.Error("a failed write ran the profile hook")
	}
	h.pollQuiet(t)
}
