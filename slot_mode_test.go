package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlotStartsActiveOnlyWhenMarkerNamesIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "active-slot")
	t.Setenv("ROUTER_STANDBY", "")
	t.Setenv("ROUTER_ACTIVE_SLOT_FILE", marker)
	t.Setenv("ROUTER_SLOT", "green")
	if !routerStartsStandby() {
		t.Fatal("slot without a marker must start standby")
	}
	if err := os.WriteFile(marker, []byte("blue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !routerStartsStandby() {
		t.Fatal("candidate slot started active while the marker names the other slot")
	}
	if err := os.WriteFile(marker, []byte("green\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if routerStartsStandby() {
		t.Fatal("serving slot restarted by KeepAlive came back standby")
	}
	t.Setenv("ROUTER_SLOT", "")
	t.Setenv("ROUTER_ACTIVE_SLOT_FILE", "")
	if routerStartsStandby() {
		t.Fatal("legacy single instance started standby")
	}
	t.Setenv("ROUTER_STANDBY", "1")
	if !routerStartsStandby() {
		t.Fatal("explicit standby ignored")
	}
}
