package privacy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func corpusRules(t testing.TB) *Rules {
	t.Helper()
	r, err := LoadRules("../../testdata/privacy/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCorpusSpans(t *testing.T) {
	r := corpusRules(t)
	m, err := newIPMapper(testIPKey, r, prfV2)
	if err != nil {
		t.Fatal(err)
	}
	d := newDetectors(r)
	for _, name := range privacySources(t) {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(privacyCorpus, name))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(privacyCorpus, name+".spans.json"))
			if err != nil {
				t.Fatal(err)
			}
			var marks []privacySpan
			if err := json.Unmarshal(data, &marks); err != nil {
				t.Fatal(err)
			}
			var want []Span
			for _, s := range marks {
				if s.Kind != "keep" && s.Kind != "public" {
					want = append(want, Span{s.Start, s.End, Kind(s.Kind), s.Text})
				}
			}
			got := d.effectiveSpans(string(body), m)
			if !reflect.DeepEqual(got, want) {
				for _, s := range got {
					found := false
					for _, w := range want {
						if s == w {
							found = true
						}
					}
					if !found {
						t.Errorf("extra: %+v", s)
					}
				}
				for _, s := range want {
					found := false
					for _, w := range got {
						if s == w {
							found = true
						}
					}
					if !found {
						t.Errorf("missing: %+v", s)
					}
				}
			}
		})
	}
}

func TestDictionaryUnicodeSimpleFold(t *testing.T) {
	rules := &Rules{Entries: []Entry{
		{Kind: KindPerson, Forms: []string{"Kelvin"}},
		{Kind: KindOrg, Forms: []string{"Ромашка"}},
		{Kind: KindProject, Forms: []string{"Sage"}},
		{Kind: KindUnit, Forms: []string{"Caf*"}},
	}}
	text := "Kelvin РОМАШКА ſage Café"
	var want []Span
	for _, tc := range []struct {
		value string
		kind  Kind
	}{
		{"Kelvin", KindPerson},
		{"РОМАШКА", KindOrg},
		{"ſage", KindProject},
		{"Café", KindUnit},
	} {
		start := strings.Index(text, tc.value)
		want = append(want, Span{start, start + len(tc.value), tc.kind, tc.value})
	}
	if got := newDetectors(rules).dict.Detect(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("dictionary casefold or stem changed: got %+v, want %+v", got, want)
	}
}

func TestSecretKeywordNegatives(t *testing.T) {
	for _, s := range []string{"PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=", "bad password", "not a secret: aGVsbG8gd29ybGQ=", "password=${DB_PASSWORD}", "password: <пароль>", "password=********", "password={{ vault.password }}"} {
		if spans := detectSecrets(s); len(spans) != 0 {
			t.Errorf("%q: %+v", s, spans)
		}
	}
}
