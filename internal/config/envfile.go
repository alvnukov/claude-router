package config

import (
	"os"
	"strconv"
	"strings"

	"localrouter/internal/platform"
)

func pick(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// ReadEnv parses the KEY=VALUE subset of shell syntax the env file uses:
// optional `export`, double quotes with Go-style escapes, single quotes verbatim.
func ReadEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, val, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		val = strings.TrimSpace(val)
		switch {
		case strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"") && len(val) >= 2:
			if u, err := strconv.Unquote(val); err == nil {
				val = u
			} else {
				val = val[1 : len(val)-1]
			}
		case strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'") && len(val) >= 2:
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out, nil
}

// WriteEnv rewrites KEY=VALUE lines in place, keeping comments and every other
// line as they are, and appends keys the file did not have. The file holds the
// local endpoint's API key, so it is written 0600 through a temp file and a
// rename rather than truncated in place.
func WriteEnv(path string, updates map[string]string) error {
	mode := os.FileMode(0o600)
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	done := map[string]bool{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, _, ok := strings.Cut(t, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok {
			continue
		}
		if v, want := updates[key]; want && !done[key] {
			lines[i] = key + "=" + envQuote(v)
			done[key] = true
		}
	}
	for _, key := range sortedKeys(updates) {
		if !done[key] {
			lines = append(lines, key+"="+envQuote(updates[key]))
		}
	}
	return platform.WriteFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), mode)
}

func envQuote(v string) string {
	if v == "" || strings.ContainsAny(v, " \t#$\"'\\") {
		return strconv.Quote(v)
	}
	return v
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
