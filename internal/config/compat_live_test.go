//go:build compat

package config

import (
	"os"
	"testing"
)

// TestConfigLiveCopyRoundTrip runs the store over a copy of a live router's
// files and fails if anything in the copy changed: a live setup that is
// already migrated has to come through activation and a no-op edit byte for
// byte. It writes into the copy, so it only builds under the compat tag and
// only runs with ROUTER_COMPAT_DIR set; the report names files, sizes and
// hashes, never their contents. From the repository root:
//
//	d=$(mktemp -d) && chmod 700 "$d" && mkdir -m 700 "$d/home"
//	cp -Rp <live dir>/providers.json <live dir>/providers.json.profiles <live dir>/providers.json.active-profile <live dir>/env "$d/"
//	go test -tags compat -c -o "$d/config.test" ./internal/config
//	HOME="$d/home" ROUTER_HOME="$d" ROUTER_PROVIDERS_FILE="$d/providers.json" ROUTER_ENV_FILE="$d/env" ROUTER_COMPAT_DIR="$d" \
//		"$d/config.test" -test.run '^TestConfigLiveCopyRoundTrip$' -test.count=1 -test.v
//	rm -rf "$d"
func TestConfigLiveCopyRoundTrip(t *testing.T) {
	dir := os.Getenv("ROUTER_COMPAT_DIR")
	if dir == "" {
		t.Skip("ROUTER_COMPAT_DIR is not set")
	}
	root, err := liveCopyDir(dir, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	// The router takes ROUTER_CLOUD_ONLY from the env file, and the pool
	// migration reads it; nothing else in the file bears on providers.json.
	vals, err := ReadEnv(os.Getenv("ROUTER_ENV_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_CLOUD_ONLY", vals["ROUTER_CLOUD_ONLY"])
	path := os.Getenv("ROUTER_PROVIDERS_FILE")

	before := treeSums(t, root)
	c, err := startup(path, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewStore(c, path)
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"save codex ids", s.SaveCodexIDs},
		{"reload", s.Reload},
		{"migrate", s.Migrate},
		{"ensure profiles", s.EnsureProfiles},
		{"update", func() error { return s.Update(func(*Local) error { return nil }) }},
	} {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	if report := treeReport(before, treeSums(t, root)); report != "" {
		t.Fatalf("the copy changed:\n%s", report)
	}
}
