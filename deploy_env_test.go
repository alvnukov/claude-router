package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchdSlotAddressesOverrideLegacyEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("ROUTER_LISTEN=127.0.0.1:8787\nROUTER_UI_LISTEN=127.0.0.1:8788\nROUTER_STANDBY=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ROUTER_LISTEN", "ROUTER_UI_LISTEN", "ROUTER_STANDBY", "ROUTER_SLOT", "ROUTER_ENV_FILE"} {
		original, set := os.LookupEnv(key)
		t.Cleanup(func() {
			if set {
				os.Setenv(key, original)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	os.Setenv("ROUTER_ENV_FILE", path)
	os.Setenv("ROUTER_SLOT", "green")
	os.Setenv("ROUTER_LISTEN", "127.0.0.1:8792")
	os.Setenv("ROUTER_UI_LISTEN", "127.0.0.1:8794")
	os.Setenv("ROUTER_STANDBY", "1")
	loadEnvFile()
	if os.Getenv("ROUTER_LISTEN") != "127.0.0.1:8792" || os.Getenv("ROUTER_UI_LISTEN") != "127.0.0.1:8794" || os.Getenv("ROUTER_STANDBY") != "1" {
		t.Fatalf("legacy env took slot ports or standby mode: %s %s %s", os.Getenv("ROUTER_LISTEN"), os.Getenv("ROUTER_UI_LISTEN"), os.Getenv("ROUTER_STANDBY"))
	}
}
