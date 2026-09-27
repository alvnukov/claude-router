package privacy

import (
	"bytes"
	"strings"
	"testing"
)

func replaceTestWord(text string, _ fieldKind) ([]textEdit, error) {
	var edits []textEdit
	for pos := 0; pos < len(text); {
		i := strings.Index(text[pos:], "Маркер")
		if i < 0 {
			break
		}
		i += pos
		edits = append(edits, textEdit{i, i + len("Маркер"), "Фантом", nil})
		pos = i + len("Маркер")
	}
	return edits, nil
}
func TestRewriteGolden(t *testing.T) {
	input := []byte(`{ "n":12345678901234567890,"huge":1e400,"system":"\u003c Маркер \ud800\n", "messages":[{"role":"user","content":"\u041c\u0430\u0440\u043a\u0435\u0440!"}] }`)
	want := bytes.ReplaceAll(input, []byte("Маркер"), []byte("Фантом"))
	want = bytes.ReplaceAll(want, []byte(`\u041c\u0430\u0440\u043a\u0435\u0440`), []byte(`\u0424\u0430\u043d\u0442\u043e\u043c`))
	got, err := rewriteRequest(input, replaceTestWord)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("got %s, error %v; want %s", got, err, want)
	}
	for _, s := range []string{`{"system":"x"`, `{"system":"x"} null`, `{"a":NaN}`} {
		if out, err := rewriteRequest([]byte(s), replaceTestWord); err == nil || out != nil {
			t.Fatalf("invalid input accepted: %q", s)
		}
	}
}
func TestStructureDiff(t *testing.T) {
	input := []byte(`{"model":"Маркер","metadata":{"user_id":"Маркер"},"tool_choice":{"name":"Маркер"},"system":"Маркер","messages":[{"role":"user","content":[{"type":"text","text":"Маркер","cache_control":{"x":"Маркер"}},{"type":"tool_use","id":"Маркер","name":"Маркер","input":{"Маркер":{"deep":["Маркер",12345678901234567890]}}},{"type":"tool_result","tool_use_id":"Маркер","content":[{"type":"text","text":"Маркер"}]},{"type":"thinking","thinking":"Маркер","signature":"Маркер"}]}],"tools":[{"name":"Маркер","description":"Маркер","input_schema":{"type":"Маркер","properties":{"Маркер":{"type":"string","description":"Маркер","title":"Маркер","enum":["Маркер"]}},"required":["Маркер"],"pattern":"Маркер"}}]}`)
	got, err := rewriteRequest(input, replaceTestWord)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "Фантом") != 9 {
		t.Fatalf("incorrect allowed paths: %s", got)
	}
	for _, kept := range []string{`"model":"Маркер"`, `"user_id":"Маркер"`, `"name":"Маркер"`, `"cache_control":{"x":"Маркер"}`, `"thinking":"Маркер"`, `"signature":"Маркер"`, `"required":["Маркер"]`, `"properties":{"Маркер":`} {
		if !bytes.Contains(got, []byte(kept)) {
			t.Errorf("changed %s", kept)
		}
	}
}
func TestBillingLineUntouched(t *testing.T) {
	input := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: Маркер\nМаркер"}]}`)
	want := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: Маркер\nФантом"}]}`)
	got, err := rewriteRequest(input, replaceTestWord)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: %v", got, err)
	}
}
