package privacy

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateMaskGolden = flag.Bool("update-privacy-golden", false, "update synthetic CLI masking golden files")

func TestMaskCommandGolden(t *testing.T) {
	for _, name := range []string{"api/messages-request.json", "network/core-sw1.cfg"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			var out, report bytes.Buffer
			args := []string{"mask", "-home", home, "-rules", "../../testdata/privacy/rules.json", filepath.Join(privacyCorpus, name)}
			opt := Options{Home: "/home/testlogin", Hostname: "test-host.local", newKey: func() ([]byte, error) { return bytes.Repeat([]byte{5}, 32), nil }}
			if err := command(context.Background(), args, &out, &report, opt); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "romashka.example") || strings.Contains(out.String(), "Ромашка") || strings.Contains(out.String(), "10.113.8.21") {
				t.Fatal("sensitive fixture value leaked")
			}

			golden := filepath.Join("../../testdata/privacy/golden", strings.ReplaceAll(name, "/", "-"))
			if *updateMaskGolden {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			expected, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), expected) {
				t.Fatal("CLI golden changed; inspect the diff before updating")
			}
			files, err := os.ReadDir(home)
			if err != nil || len(files) != 0 {
				t.Fatal("preview wrote state", err)
			}
			var repeat, secondReport bytes.Buffer
			if err := command(context.Background(), args, &repeat, &secondReport, opt); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), repeat.Bytes()) || !bytes.Equal(report.Bytes(), secondReport.Bytes()) {
				t.Fatal("preview not deterministic")
			}
			if !strings.Contains(report.String(), "scope=request") {
				t.Fatal("missing report")
			}
		})
	}
}
