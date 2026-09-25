package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkScript runs deploy/check.sh with go, launchctl, caddy and curl
// replaced by stubs that only record their calls. The stub build answers
// service-labels with labelsJSON and `ports` with ports; launchctl print
// succeeds only for the labels in loaded.
func checkScript(t *testing.T, env []string, labelsJSON, ports string, loaded ...string) (string, string, error) {
	t.Helper()
	return checkScriptArgs(t, nil, env, labelsJSON, ports, loaded...)
}

// checkScriptArgs is checkScript with arguments for the script. curl answers
// with CURL_OUT when env sets it.
func checkScriptArgs(t *testing.T, args, env []string, labelsJSON, ports string, loaded ...string) (string, string, error) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	for _, dir := range []string{bin, home, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(root, "calls")
	stub("go", `printf 'go %s\n' "$*" >> "$CALLS"
while [ "$1" != "-o" ]; do shift; done
cat > "$2" <<'BIN'
#!/bin/sh
printf 'binary %s\n' "$*" >> "$CALLS"
case "$1" in
  service-labels) printf '%s\n' "$LABELS_JSON" ;;
  ports) printf '%s\n' "$PORTS" ;;
  upstream) printf '127.0.0.1:1\n' ;;
esac
BIN
chmod +x "$2"`)
	stub("launchctl", `printf 'launchctl %s\n' "$*" >> "$CALLS"
if [ "$1" = print ]; then
  for label in $LOADED; do [ "$2" = "gui/$(id -u)/$label" ] && exit 0; done
  exit 113
fi
[ "$1" != bootstrap ]`)
	stub("caddy", `printf 'caddy %s\n' "$*" >> "$CALLS"`)
	stub("curl", `printf 'curl %s\n' "$*" >> "$CALLS"; [ -n "$CURL_OUT" ] || exit 7; printf '%s' "$CURL_OUT"`)
	cmd := exec.Command("bash", append([]string{filepath.Join("deploy", "check.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home, "TMPDIR="+filepath.Join(root, "tmp"), "PATH="+bin+":"+os.Getenv("PATH"),
		"CALLS="+calls, "LABELS_JSON="+labelsJSON, "PORTS="+ports, "LOADED="+strings.Join(loaded, " "))
	cmd.Env = append(cmd.Env, env...)
	output, err := cmd.CombinedOutput()
	data, _ := os.ReadFile(calls)
	return string(output), string(data), err
}

func scratchLabels(prefix string, live bool) string {
	return fmt.Sprintf(`{"blue":"%[1]s.blue","green":"%[1]s.green","caddy":"%[1]s.caddy","prefix":"%[1]s","default_prefix":%[2]t}`, prefix, live)
}

func freeTestPorts(t *testing.T, n int) []string {
	t.Helper()
	var ports []string
	for range n {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		ports = append(ports, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
	}
	return ports
}

func touchedLaunchd(calls string) bool {
	for _, verb := range []string{"bootstrap", "bootout", "kickstart", "load", "unload", "enable", "disable", "remove", "submit"} {
		if strings.Contains(calls, "launchctl "+verb) {
			return true
		}
	}
	return false
}

func TestCheckScriptRefusesLiveLabelsWithoutLiveFlag(t *testing.T) {
	ports := strings.Join(freeTestPorts(t, 7), " ")
	output, calls, err := checkScript(t, []string{"CHECK_LABEL_PREFIX=" + defaultRouterLabel}, scratchLabels(defaultRouterLabel, true), ports)
	if err == nil || !strings.Contains(output, "--live") || strings.Contains(calls, "launchctl") {
		t.Fatalf("check ran against the live labels: err=%v\n%s\n%s", err, output, calls)
	}
}

func TestCheckScriptRefusesBeforeLaunchdWhenScratchIsNotItsOwn(t *testing.T) {
	const prefix = "router-check.test"
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free := freeTestPorts(t, 6)
	withPort := func(port string) string { return strings.Join(append(free[:6:6], port), " ") }
	routerHome := t.TempDir()
	for _, tc := range []struct {
		name, ports string
		env         []string
		loaded      []string
	}{
		{name: "legacy label loaded", ports: withPort(freeTestPorts(t, 1)[0]), loaded: []string{prefix}},
		{name: "slot label loaded", ports: withPort(freeTestPorts(t, 1)[0]), loaded: []string{prefix + ".green"}},
		{name: "Caddy label loaded", ports: withPort(freeTestPorts(t, 1)[0]), loaded: []string{prefix + ".caddy"}},
		{name: "live API port", ports: withPort("8787")},
		{name: "live slot port", ports: withPort("8793")},
		{name: "port in use", ports: withPort(fmt.Sprint(busy.Addr().(*net.TCPAddr).Port))},
		{name: "too few ports", ports: strings.Join(free[:5], " ")},
		{name: "scratch inside router home", ports: withPort(freeTestPorts(t, 1)[0]), env: []string{"TMPDIR=" + routerHome, "ROUTER_HOME=" + routerHome}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := append([]string{"CHECK_LABEL_PREFIX=" + prefix}, tc.env...)
			output, calls, err := checkScript(t, env, scratchLabels(prefix, false), tc.ports, tc.loaded...)
			if err == nil || touchedLaunchd(calls) {
				t.Fatalf("check did not stop before launchd: err=%v\n%s\n%s", err, output, calls)
			}
		})
	}
}

// --live redeploys the build the installed router runs, never the checkout
// the script lies in. Everything here is a stub under a temporary home.
func TestCheckScriptLiveRedeploysTheInstalledBuild(t *testing.T) {
	home := t.TempDir()
	labels := scratchLabels(defaultRouterLabel, true)
	files := map[string]string{
		"deploy.json":      `{"public_api":"127.0.0.1:1"}`,
		"active-slot":      "blue\n",
		"localrouter.blue": "#!/bin/sh\nprintf 'binary %s\\n' \"$*\" >> \"$CALLS\"\n[ \"$1\" != service-labels ] || printf '%s\\n' \"$LABELS_JSON\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"ROUTER_HOME=" + home, `CURL_OUT={"slot":"blue","pid":1}`}
	output, calls, _ := checkScriptArgs(t, []string{"--live"}, env, labels, "")
	binary := filepath.Join(home, "localrouter.blue")
	if !strings.Contains(calls, "binary deploy -home "+home+" -binary "+binary+" -force\n") || strings.Contains(calls, "go build") {
		t.Fatalf("live check deployed something other than the installed build:\n%s\n%s", output, calls)
	}
}

func TestCheckScriptTouchesLaunchdOnlyAfterItsPreflight(t *testing.T) {
	const prefix = "router-check.test"
	ports := strings.Join(freeTestPorts(t, 7), " ")
	output, calls, err := checkScript(t, []string{"CHECK_LABEL_PREFIX=" + prefix}, scratchLabels(prefix, false), ports)
	if err == nil {
		t.Fatalf("stub bootstrap succeeded? %s", output)
	}
	first := strings.Index(calls, "launchctl bootstrap")
	if first < 0 {
		t.Fatalf("preflight refused a clean scratch run: %s\n%s", output, calls)
	}
	for _, label := range []string{prefix, prefix + ".blue", prefix + ".green", prefix + ".caddy"} {
		if !strings.Contains(calls[:first], "launchctl print gui/") || !strings.Contains(calls[:first], "/"+label+"\n") {
			t.Fatalf("label %s not checked before the first bootstrap:\n%s", label, calls)
		}
	}
	if strings.Contains(calls, "launchctl bootout") {
		t.Fatalf("cleanup unloaded labels it never loaded:\n%s", calls)
	}
}
