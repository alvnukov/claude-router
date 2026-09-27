package privacy

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var updatePrivacyCorpus = flag.Bool("update-privacy-corpus", false, "rewrite testdata/privacy/corpus from testdata/privacy/src")

const (
	privacySrc    = "../../testdata/privacy/src"
	privacyCorpus = "../../testdata/privacy/corpus"
	privacyOpen   = "⟦"
	privacyClose  = "⟧"
)

var privacyKinds = []string{
	"ipv4", "ipv6", "cidr4", "cidr6", "mac", "host", "email", "org",
	"person", "address", "phone", "secret", "public", "keep",
}

// privacySpan is one expected span: byte offsets into the corpus file.
type privacySpan struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
}

// stripPrivacyMarks removes ⟦kind:value⟧ marks and returns the plain text
// with the spans of the marked values.
func stripPrivacyMarks(src string) (string, []privacySpan, error) {
	var out strings.Builder
	var spans []privacySpan
	for {
		i := strings.Index(src, privacyOpen)
		if i < 0 {
			if strings.Contains(src, privacyClose) {
				return "", nil, fmt.Errorf("stray %s", privacyClose)
			}
			out.WriteString(src)
			return out.String(), spans, nil
		}
		if strings.Contains(src[:i], privacyClose) {
			return "", nil, fmt.Errorf("stray %s before offset %d", privacyClose, out.Len()+i)
		}
		out.WriteString(src[:i])
		src = src[i+len(privacyOpen):]
		j := strings.Index(src, privacyClose)
		if j < 0 {
			return "", nil, fmt.Errorf("unclosed %s at offset %d", privacyOpen, out.Len())
		}
		mark := src[:j]
		src = src[j+len(privacyClose):]
		if strings.Contains(mark, privacyOpen) {
			return "", nil, fmt.Errorf("nested mark at offset %d", out.Len())
		}
		kind, text, ok := strings.Cut(mark, ":")
		if !ok || text == "" {
			return "", nil, fmt.Errorf("mark %q at offset %d has no kind:value", mark, out.Len())
		}
		if !slices.Contains(privacyKinds, kind) {
			return "", nil, fmt.Errorf("unknown kind %q at offset %d", kind, out.Len())
		}
		spans = append(spans, privacySpan{Start: out.Len(), End: out.Len() + len(text), Kind: kind, Text: text})
		out.WriteString(text)
	}
}

func privacySources(t *testing.T) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(privacySrc, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(privacySrc, path)
		names = append(names, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no privacy corpus sources")
	}
	return names
}

func TestPrivacyCorpus(t *testing.T) {
	kinds := map[string]int{}
	for _, name := range privacySources(t) {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(privacySrc, name))
			if err != nil {
				t.Fatal(err)
			}
			text, spans, err := stripPrivacyMarks(string(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(spans) == 0 {
				t.Fatal("no marked values")
			}
			for _, s := range spans {
				kinds[s.Kind]++
			}
			if strings.HasSuffix(name, ".json") && !json.Valid([]byte(text)) {
				t.Fatal("stripped text is not valid JSON")
			}
			spansJSON, err := json.MarshalIndent(spans, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			spansJSON = append(spansJSON, '\n')

			corpusPath := filepath.Join(privacyCorpus, filepath.FromSlash(name))
			spansPath := corpusPath + ".spans.json"
			if *updatePrivacyCorpus {
				if err := os.MkdirAll(filepath.Dir(corpusPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(corpusPath, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(spansPath, spansJSON, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			corpus, err := os.ReadFile(corpusPath)
			if err != nil {
				t.Fatalf("%v (regenerate with -update-privacy-corpus)", err)
			}
			if string(corpus) != text {
				t.Fatalf("%s is stale: regenerate with -update-privacy-corpus", corpusPath)
			}
			gotSpans, err := os.ReadFile(spansPath)
			if err != nil {
				t.Fatalf("%v (regenerate with -update-privacy-corpus)", err)
			}
			if !bytes.Equal(gotSpans, spansJSON) {
				t.Fatalf("%s is stale: regenerate with -update-privacy-corpus", spansPath)
			}
			for _, s := range spans {
				if string(corpus[s.Start:s.End]) != s.Text {
					t.Fatalf("span %d..%d is %q, want %q", s.Start, s.End, corpus[s.Start:s.End], s.Text)
				}
			}
		})
	}
	for _, kind := range privacyKinds {
		if kinds[kind] == 0 {
			t.Errorf("no %s spans in the corpus", kind)
		}
	}
}

// TestPrivacyCorpusOrphans fails when corpus/ holds a file with no source.
func TestPrivacyCorpusOrphans(t *testing.T) {
	want := map[string]bool{}
	for _, name := range privacySources(t) {
		want[name] = true
		want[name+".spans.json"] = true
	}
	err := filepath.WalkDir(privacyCorpus, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(privacyCorpus, path)
		if err != nil {
			return err
		}
		if !want[filepath.ToSlash(rel)] {
			t.Errorf("%s has no source in %s", path, privacySrc)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStripPrivacyMarks(t *testing.T) {
	text, spans, err := stripPrivacyMarks("ip ⟦ipv4:10.0.0.1⟧ и ⟦org:Ромашки⟧.")
	if err != nil {
		t.Fatal(err)
	}
	if text != "ip 10.0.0.1 и Ромашки." {
		t.Fatalf("text = %q", text)
	}
	want := []privacySpan{
		{Start: 3, End: 11, Kind: "ipv4", Text: "10.0.0.1"},
		{Start: 15, End: 29, Kind: "org", Text: "Ромашки"},
	}
	if !slices.Equal(spans, want) {
		t.Fatalf("spans = %+v, want %+v", spans, want)
	}
	for _, bad := range []string{"⟦ipv4:1.2.3.4", "1⟧", "⟦nope:x⟧", "⟦ipv4⟧", "⟦ipv4:⟦ipv4:1⟧⟧"} {
		if _, _, err := stripPrivacyMarks(bad); err == nil {
			t.Errorf("stripPrivacyMarks(%q) accepted a bad mark", bad)
		}
	}
}

// TestCorpusFakeKeysInvalid guards against a corpus key that a secret
// scanner would take for a live one: the GitHub token fails its checksum,
// the Anthropic key lacks its AA tail, and AWS keys are the documentation
// examples.
func TestCorpusFakeKeysInvalid(t *testing.T) {
	var all strings.Builder
	for _, name := range privacySources(t) {
		b, err := os.ReadFile(filepath.Join(privacyCorpus, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
		all.WriteByte('\n')
	}
	checks := []struct {
		re    *regexp.Regexp
		valid func(key string) bool
	}{
		{regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+`), func(k string) bool {
			return k != "https://hooks.slack.com/services/FAKE/FAKE/INVALID"
		}},
		{regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`), githubChecksumValid},
		{regexp.MustCompile(`sk-ant-api03-[A-Za-z0-9_-]+`), func(k string) bool { return strings.HasSuffix(k, "AA") }},
		{regexp.MustCompile(`AKIA[A-Z0-9]{16}`), func(k string) bool { return k != "AKIAIOSFODNN7EXAMPLE" }},
	}
	for _, c := range checks {
		keys := c.re.FindAllString(all.String(), -1)
		if len(keys) == 0 {
			t.Errorf("no key matches %s in the corpus", c.re)
		}
		for _, k := range keys {
			if c.valid(k) {
				t.Errorf("corpus key %s… looks valid", k[:8])
			}
		}
	}
}

// githubChecksumValid reports whether the last six characters of a ghp_
// token are the base62 CRC32 of the characters before them, in either
// base62 alphabet order, with or without the prefix.
func githubChecksumValid(token string) bool {
	tail := token[len(token)-6:]
	for _, alphabet := range []string{
		"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
		"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	} {
		for _, in := range []string{token[4 : len(token)-6], token[:len(token)-6]} {
			n := crc32.ChecksumIEEE([]byte(in))
			var sum [6]byte
			for i := 5; i >= 0; i-- {
				sum[i] = alphabet[n%62]
				n /= 62
			}
			if string(sum[:]) == tail {
				return true
			}
		}
	}
	return false
}
