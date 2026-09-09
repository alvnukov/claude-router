package main

import "testing"

// The routing decision now decides whether a prompt leaves the machine, so it
// is worth a test of its own.
func TestIsLocal(t *testing.T) {
	base := config{local: oneProvider("http://h/v1", "local-model")}
	cloudOnly := config{local: oneProvider("http://h/v1", "local-model"), cloudOnly: []string{"claude-opus-5"}}

	cases := []struct {
		cfg   config
		model string
		want  bool
	}{
		{base, "local-model", true},
		{base, "local-fable", true},
		{base, "claude-sonnet-5", false},
		{base, "claude-opus-5", false},
		{base, "", false},

		{cloudOnly, "claude-opus-5", false},
		{cloudOnly, "claude-opus-5-20260501", false},
		{cloudOnly, "claude-sonnet-5", true},
		{cloudOnly, "claude-haiku-4-5-20251001", true},
		{cloudOnly, "claude-opus-4-5-20251101", true},
		{cloudOnly, "local-model", true},
		{cloudOnly, "", false},
	}
	for _, c := range cases {
		if got := c.cfg.isLocal(c.model); got != c.want {
			t.Errorf("isLocal(%q) with cloudOnly=%v = %v, want %v", c.model, c.cfg.cloudOnly, got, c.want)
		}
	}
}
