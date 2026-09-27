package privacy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGeneratedRoundTrips(t *testing.T) {
	rules, err := ParseRules([]byte(`{"fields":[{"path":"messages[*].content[*].input.accounts[*].login","kind":"login"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, rules)
	e.store.newKey = func() ([]byte, error) { return bytes.Clone(testIPKey), nil }
	random := rand.New(rand.NewPCG(41, 73))
	accounts := make([]map[string]string, 10000)
	alphabet := []rune("aAzZ09ЖяЁ_'\\\"/[]")
	for i := range accounts {
		var value strings.Builder
		for range 14 {
			value.WriteRune(alphabet[random.IntN(len(alphabet))])
		}
		accounts[i] = map[string]string{"login": fmt.Sprintf("Account%d_%s", i, value.String()), "password": fmt.Sprintf("FAKE-%d-%s", i, value.String())}
	}
	body, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "tool_use", "input": map[string]any{"accounts": accounts}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	masked, req := mustMask(t, e, body)
	if bytes.Contains(masked, []byte("Account")) || bytes.Contains(masked, []byte("FAKE-")) {
		t.Fatal("generated credentials leaked")
	}
	restored, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(restored, body) {
		t.Fatal("generated roundtrip failed", err)
	}
}

func TestLargeRepeatedContext(t *testing.T) {
	// Explicit byte size, not a claim that bytes equal tokens. This exceeds the
	// usual UTF-8 size of 400k tokens and exercises repeated history in one body.
	text := strings.Repeat("Task context repeats without new entities. ", 100000) + "\nРомашка 10.1.2.3"
	e := testEngine(t, corpusRules(t))
	body := requestBody("large", text)
	a, req := mustMask(t, e, body)
	b, _ := mustMask(t, e, body)
	if !bytes.Equal(a, b) {
		t.Fatal("same history changed pseudonyms")
	}
	back, err := e.UnmaskJSON(req, a)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatal("large context not reversible", err)
	}
}

func TestEncodedStructuredSecretOrder(t *testing.T) {
	const secret = "FAKE-value-without-signature"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	body := []byte(`{"messages":[{"content":[{"type":"tool_use","input":{"output":"` + encoded + `","password":"` + secret + `"}}]}]}`)
	e := testEngine(t, corpusRules(t))
	masked, _ := mustMask(t, e, body)
	if bytes.Contains(masked, []byte(encoded)) {
		t.Fatal("field order bypassed encoded secret detection")
	}
}

func FuzzJSONRewriteRoundTrip(f *testing.F) {
	f.Add("Ромашка", "before", "after")
	f.Add("Ab.cdefghiJ", "\\u0410", "\"\\/")
	f.Fuzz(func(t *testing.T, value, left, right string) {
		if value == "" || len(value) > 512 || len(left)+len(right) > 2048 {
			t.Skip()
		}
		body, _ := json.Marshal(map[string]string{"system": left + value + right})
		masked, err := rewriteRequest(body, func(text string, _ fieldKind) ([]textEdit, error) {
			at := strings.Index(text, value)
			if at < 0 {
				return nil, nil
			}
			return []textEdit{{at, at + len(value), "REPLACEMENT", nil}}, nil
		})
		if !utf8.ValidString(value) {
			original, _ := lookupString(body, "system")
			if strings.Contains(original, value) && (err == nil || masked != nil) {
				t.Fatal("partial UTF-8 rune edit was not rejected")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(masked) {
			t.Fatal("invalid rewritten JSON")
		}
		original, _ := lookupString(body, "system")
		got, _ := lookupString(masked, "system")
		at := strings.Index(original, value)
		if at >= 0 && got != original[:at]+"REPLACEMENT"+original[at+len(value):] {
			t.Fatal("unrelated bytes changed")
		}
	})
}

func BenchmarkMaskRepeatedContext(b *testing.B) {
	e := testEngine(b, corpusRules(b))
	body := requestBody("benchmark", strings.Repeat("Routine context with no new entity. ", 600)+"Ромашка 10.1.2.3")
	_, req, err := e.Mask(body)
	if err != nil {
		b.Fatal(err)
	}
	req.Close()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, req, err := e.Mask(body)
		if err != nil {
			b.Fatal(err)
		}
		req.Close()
	}
}

func BenchmarkMask1MB(b *testing.B) {
	e := testEngine(b, corpusRules(b))
	body := requestBody("benchmark-1mb", strings.Repeat("Routine context with no new entity. ", 30000)+"Ромашка 10.1.2.3")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, req, err := e.Mask(body)
		if err != nil {
			b.Fatal(err)
		}
		req.Close()
	}
}
