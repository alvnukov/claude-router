package privacy

import (
	"bytes"
	"testing"
)

func TestConfigurableRules(t *testing.T) {
	r, err := ParseRules([]byte(`{"patterns":[{"name":"equipment-login","kind":"person","regex":"(?i)login=([a-z][a-z0-9_-]+)","group":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	body := requestBody("policy", "login=technician_27")
	masked, req := mustMask(t, e, body)
	if bytes.Contains(masked, []byte("technician_27")) || !bytes.Contains(masked, []byte("login=")) {
		t.Fatal("custom capture rule ignored")
	}
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatal("custom rule roundtrip", err)
	}
	for _, bad := range []string{`{"detection":{"ipv4":"("}}`, `{"detection":{"assignment":".*"}}`, `{"patterns":[{"name":"bad","kind":"person","regex":"(","group":0}]}`, `{"patterns":[{"name":"bad","kind":"person","regex":".*","group":2}]}`, `{"patterns":[{"name":"bad","kind":"bogus","regex":"x","group":0}]}`} {
		if _, err := ParseRules([]byte(bad)); err == nil {
			t.Fatal("invalid policy accepted", bad)
		}
	}
}
func TestAllowPathsSymmetric(t *testing.T) {
	r, err := ParseRules([]byte(`{"allow_paths":["messages[*].content"]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	body := []byte(`{"messages":[{"role":"user","content":"10.1.2.3"}],"system":"10.1.2.3"}`)
	masked, req := mustMask(t, e, body)
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatal("allowed path changed on response", err)
	}
}
