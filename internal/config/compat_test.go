package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Compatibility of the store's files across the move of this code out of
// package main. The code before the move ran ten operations on the synthetic
// fixture in testdata/compat and dumped the files after each one into
// golden/NN-op, with a TREE listing of mode, size and path; the moved code
// runs the same operations and wants the same files, byte for byte:
//
//	go test -run TestConfigCompatFiles -count=1 ./internal/config
//
// A copy of the live files is checked by TestConfigLiveCopyRoundTrip, under
// the compat tag.
func TestConfigCompatFiles(t *testing.T) {
	src := filepath.Join("testdata", "compat")
	work := t.TempDir()
	for _, dir := range []string{"home", "legacy"} {
		compatCopy(t, filepath.Join(src, dir), filepath.Join(work, dir))
	}
	for _, key := range []string{"ROUTER_CLOUD_ONLY", "ROUTER_LOCAL_API_KEY", "ROUTER_LOCAL_MODELS", "ROUTER_SLOT"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", work)

	load := func(dir string) *Store {
		t.Helper()
		path := filepath.Join(work, dir, "providers.json")
		t.Setenv("ROUTER_PROVIDERS_FILE", path)
		t.Setenv("ROUTER_ENV_FILE", filepath.Join(work, dir, "env"))
		c, err := startup(path, false)
		if err != nil {
			t.Fatalf("load %s: %v", dir, err)
		}
		s := NewStore(c, path)
		// The settings come from the env file, as they do at start.
		if _, err := s.reloadEnv(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return s
	}
	step := 0
	check := func(op string, roots ...string) {
		t.Helper()
		step++
		compatCheck(t, work, filepath.Join(src, "golden", fmt.Sprintf("%02d-%s", step, op)), roots)
	}
	must := func(op string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}

	legacy := load("legacy")
	home := load("home")
	check("load", "home", "legacy")

	must("legacy profiles", legacy.EnsureProfiles())
	must("home profiles", home.EnsureProfiles())
	check("ensure-profiles", "home", "legacy")

	// Dropping lab/old-coder leaves the inactive cloud profile pointing at it.
	must("update", home.Update(func(l *Local) error {
		var models []Model
		for _, m := range l.Models {
			if m.Key() != "lab/old-coder" {
				models = append(models, m)
			}
		}
		l.Models = append(models, Model{Provider: "lab", Model: "qwen-coder-next"})
		return nil
	}))
	check("update", "home")

	must("pool settings", home.SavePoolSettings("work", PoolSettings{Type: PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15, MaxInputChars: 120000}))
	check("pool-settings", "home")

	must("create profile", home.CreateProfile("night", true))
	check("create-profile", "home")

	must("activate profile", home.ActivateProfile("night"))
	check("activate-profile", "home")

	// What the router's listing of the fixture's providers returns: the Codex
	// catalog without its hidden model, the lab's list, HTTP 500 from down.
	now := time.Now()
	probes := map[string]ProviderProbe{
		"codex": {OK: true, At: now, Models: []CatalogModel{
			{ID: "gpt-6-sol", Name: "GPT-6 Sol", Efforts: []string{"low", "high"}},
			{ID: "gpt-6.1-sol", Name: "GPT-6.1 Sol", Efforts: []string{"low", "high"}},
			{ID: "gpt-6-luna", Name: "GPT-6 Luna", Efforts: []string{"medium"}},
		}},
		"lab":  {OK: true, At: now, Models: []CatalogModel{{ID: "qwen-coder"}, {ID: "qwen-coder-next"}}},
		"down": {Msg: "HTTP 500", At: now},
	}
	must("catalog", home.UpdateCatalog(home.Get().Local, []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5"}, nil, probes, nil))
	check("catalog", "home")

	must("settings", home.UpdateSettings(func(in *SettingsInput) error {
		in.MaxInputChars = "150000"
		return nil
	}))
	must("failover", home.SetFailover(false))
	check("settings", "home")

	must("delete profile", home.DeleteProfile("cloud"))
	check("delete-profile", "home")

	must("reload", home.Reload())
	check("reload", "home")
}

// compatCopy copies the fixture with the modes the router uses: 0700
// directories, 0600 files.
func compatCopy(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var compatTimes = regexp.MustCompile(`"(checked_at|anthropic_updated|updated_at)": "[^"]*"`)

// compatNormalize replaces the catalog times a refresh takes from the clock;
// the fixture's own time and the zero time stay as written.
func compatNormalize(data []byte) []byte {
	return compatTimes.ReplaceAllFunc(data, func(m []byte) []byte {
		if bytes.HasSuffix(m, []byte(`"2026-09-01T10:00:00Z"`)) || bytes.HasSuffix(m, []byte(`"0001-01-01T00:00:00Z"`)) {
			return m
		}
		key, _, _ := bytes.Cut(m, []byte(":"))
		return append(append([]byte(nil), key...), `: "NOW"`...)
	})
}

// compatCheck wants the files under roots to be the ones in the golden
// directory dir: the same TREE listing and, once the catalog times are
// replaced, the same bytes.
func compatCheck(t *testing.T, work, dir string, roots []string) {
	t.Helper()
	var tree bytes.Buffer
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(work, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(work, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if d.IsDir() {
				fmt.Fprintf(&tree, "%s - %s\n", info.Mode(), filepath.ToSlash(rel))
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			data = compatNormalize(data)
			fmt.Fprintf(&tree, "%s %d %s\n", info.Mode(), len(data), filepath.ToSlash(rel))
			want, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				t.Errorf("%s: %v", filepath.ToSlash(rel), err)
			} else if !bytes.Equal(data, want) {
				t.Errorf("%s differs from %s:\n%s", filepath.ToSlash(rel), dir, compatFirstDiff(data, want))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.Join(dir, "TREE"))
	if err != nil {
		t.Fatal(err)
	}
	if got := compatModes(tree.Bytes()); !bytes.Equal(got, compatModes(want)) {
		t.Errorf("%s/TREE differs:\n got:\n%s\nwant:\n%s", dir, got, compatModes(want))
	}
}

// compatModes drops the mode column on Windows, which has no Unix modes;
// there only sizes and paths are compared.
func compatModes(tree []byte) []byte {
	if runtime.GOOS != "windows" {
		return tree
	}
	var out []byte
	for _, line := range bytes.SplitAfter(tree, []byte("\n")) {
		if _, rest, ok := bytes.Cut(line, []byte(" ")); ok {
			line = rest
		}
		out = append(out, line...)
	}
	return out
}

// compatFirstDiff shows the first line that differs.
func compatFirstDiff(got, want []byte) string {
	g, w := bytes.Split(got, []byte("\n")), bytes.Split(want, []byte("\n"))
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl []byte
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if !bytes.Equal(gl, wl) {
			return "line " + strconv.Itoa(i+1) + "\n got: " + string(gl) + "\nwant: " + string(wl)
		}
	}
	return "equal lines, different bytes"
}

// fileSum is all the live copy check keeps of a file: never its content.
type fileSum struct {
	size int64
	sum  [sha256.Size]byte
}

// treeSums sums every file under dir, keyed by its slash path relative to dir.
func treeSums(t *testing.T, dir string) map[string]fileSum {
	t.Helper()
	sums := map[string]fileSum{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sums[filepath.ToSlash(rel)] = fileSum{size: int64(len(data)), sum: sha256.Sum256(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

// treeReport lists the files that differ between two sums of a tree, a new or
// a missing file included, by name, size and sha256 only: the live copy holds
// keys. Equal trees give "".
func treeReport(before, after map[string]fileSum) string {
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var b strings.Builder
	for _, name := range sorted {
		was, had := before[name]
		now, has := after[name]
		switch {
		case !has:
			fmt.Fprintf(&b, "missing %s: %d %x\n", name, was.size, was.sum)
		case !had:
			fmt.Fprintf(&b, "new %s: %d %x\n", name, now.size, now.sum)
		case was != now:
			fmt.Fprintf(&b, "changed %s: %d %x -> %d %x\n", name, was.size, was.sum, now.size, now.sum)
		}
	}
	return b.String()
}

// liveCopyDir resolves ROUTER_COMPAT_DIR for the live copy check and refuses
// a directory the check could write past: it has to be a 0700 directory
// strictly under the temp dir, and HOME, ROUTER_HOME, ROUTER_PROVIDERS_FILE
// and ROUTER_ENV_FILE all have to point into it. Paths are compared with
// symlinks resolved, so each of them has to exist.
func liveCopyDir(dir string, getenv func(string) string) (string, error) {
	root, err := realPath(dir)
	if err != nil {
		return "", fmt.Errorf("ROUTER_COMPAT_DIR: %w", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("ROUTER_COMPAT_DIR %s is not a 0700 directory", dir)
	}
	tmp, err := realPath(os.TempDir())
	if err != nil {
		return "", err
	}
	if rel, ok := within(tmp, root); !ok || rel == "." {
		return "", fmt.Errorf("ROUTER_COMPAT_DIR %s is not under %s", dir, os.TempDir())
	}
	for _, key := range []string{"HOME", "ROUTER_HOME", "ROUTER_PROVIDERS_FILE", "ROUTER_ENV_FILE"} {
		p, err := realPath(getenv(key))
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		if _, ok := within(root, p); !ok {
			return "", fmt.Errorf("%s=%s is outside ROUTER_COMPAT_DIR", key, getenv(key))
		}
	}
	return root, nil
}

func realPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("not set")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// within gives p relative to root and whether p is root or lies under it.
func within(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func TestTreeReportHidesContent(t *testing.T) {
	dir := t.TempDir()
	lines := []string{`{"api_key": "sk-test-MARKER"}`, "ROUTER_LOCAL_API_KEY=sk-test-MARKER"}
	for name, data := range map[string]string{"providers.json": lines[0], "env": lines[1], "gone": lines[1]} {
		writeRaw(t, filepath.Join(dir, name), data+"\n")
	}
	before := treeSums(t, dir)
	writeRaw(t, filepath.Join(dir, "providers.json"), lines[0]+"\n"+lines[0]+"\n")
	writeRaw(t, filepath.Join(dir, "new"), lines[1]+"\n")
	if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}

	report := treeReport(before, treeSums(t, dir))
	for _, want := range []string{"changed providers.json: ", "new new: ", "missing gone: "} {
		if !strings.Contains(report, want) {
			t.Errorf("report misses %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "env:") {
		t.Errorf("report lists an unchanged file:\n%s", report)
	}
	if strings.Contains(report, "sk-test-MARKER") {
		t.Errorf("report shows a key:\n%s", report)
	}
	for _, line := range lines {
		if strings.Contains(report, line) {
			t.Errorf("report shows the line %q", line)
		}
	}
	if treeReport(before, before) != "" {
		t.Error("equal trees gave a report")
	}
}

func TestLiveCopyDirRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the live copy is checked on macOS; Windows has no 0700 directories")
	}
	dir := filepath.Join(t.TempDir(), "copy")
	outside := t.TempDir()
	for _, d := range []string{dir, filepath.Join(dir, "home")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(dir, "providers.json"), filepath.Join(dir, "env"), filepath.Join(outside, "providers.json"), filepath.Join(filepath.Dir(dir), "env")} {
		writeRaw(t, p, "{}\n")
	}
	if err := os.Symlink(filepath.Join(outside, "providers.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(over map[string]string) func(string) string {
		vars := map[string]string{"HOME": filepath.Join(dir, "home"), "ROUTER_HOME": dir,
			"ROUTER_PROVIDERS_FILE": filepath.Join(dir, "providers.json"), "ROUTER_ENV_FILE": filepath.Join(dir, "env")}
		for k, v := range over {
			vars[k] = v
		}
		return func(k string) string { return vars[k] }
	}

	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := liveCopyDir(dir, env(nil)); err != nil || got != want {
		t.Fatalf("a proper copy: %q, %v; want %q", got, err, want)
	}
	for name, c := range map[string]struct {
		dir  string
		over map[string]string
	}{
		"the temp dir itself":            {os.TempDir(), nil},
		"not 0700":                       {open, nil},
		"missing":                        {filepath.Join(dir, "none"), nil},
		"HOME outside":                   {dir, map[string]string{"HOME": outside}},
		"ROUTER_HOME outside":            {dir, map[string]string{"ROUTER_HOME": outside}},
		"ROUTER_PROVIDERS_FILE outside":  {dir, map[string]string{"ROUTER_PROVIDERS_FILE": filepath.Join(outside, "providers.json")}},
		"ROUTER_PROVIDERS_FILE via link": {dir, map[string]string{"ROUTER_PROVIDERS_FILE": filepath.Join(dir, "link.json")}},
		"ROUTER_ENV_FILE unset":          {dir, map[string]string{"ROUTER_ENV_FILE": ""}},
		"ROUTER_ENV_FILE above":          {dir, map[string]string{"ROUTER_ENV_FILE": filepath.Join(dir, "..", "env")}},
	} {
		if got, err := liveCopyDir(c.dir, env(c.over)); err == nil {
			t.Errorf("%s: accepted as %q", name, got)
		}
	}
}
