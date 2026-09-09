package main

import "testing"

// Templates are parsed at startup only; a stray {{end}} must fail here,
// not when launchd restarts the router.
func TestTemplatesParse(t *testing.T) {
	if _, err := uiTemplates(); err != nil {
		t.Fatal(err)
	}
}
