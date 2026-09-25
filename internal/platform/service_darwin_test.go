package platform

import (
	"path/filepath"
	"testing"
)

func TestNewServiceIsLaunchdInUserAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	svc, err := NewService()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := svc.(Launchd)
	if !ok {
		t.Fatalf("NewService() = %T; want Launchd", svc)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents"); l.Dir != want || l.Domain != LaunchdDomain() || l.Run == nil {
		t.Fatalf("NewService() = %+v; want Dir %s, Domain %s, a runner", l, want, LaunchdDomain())
	}
}
