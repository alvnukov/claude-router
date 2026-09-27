package privacy

import (
	"bytes"
	"testing"
)

func TestFilterSwitchesAndSnapshot(t *testing.T) {
	r, err := ParseRules([]byte(`{"filters":{"ipv4":false,"dictionary":false},"entries":[{"kind":"person","forms":["Zorvex"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	body := requestBody("", "Zorvex 10.2.3.4\npassword=shh-SYNTHETIC-secret")
	masked, req := mustMask(t, e, body)
	if !bytes.Contains(masked, []byte("Zorvex 10.2.3.4")) || bytes.Contains(masked, []byte("shh-SYNTHETIC-secret")) {
		t.Fatal("filter switches ignored")
	}
	r.Filters["secret"] = false
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatal("snapshot roundtrip", err)
	}
	if _, err := ParseRules([]byte(`{"filters":{"typo":false}}`)); err == nil {
		t.Fatal("unknown filter accepted")
	}
}
