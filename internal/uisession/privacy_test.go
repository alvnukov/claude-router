package uisession

import (
	"os"
	"path/filepath"
	"testing"

	"localrouter/internal/privacy"
)

func TestPromptTitles(t *testing.T) {
	profiles := func(enabled string) string {
		return `{"version":1,"enabled":` + enabled + `,"default":"safe","profiles":[{"id":"safe","name":"Safe","enabled":true,"rules":{}}],"bindings":[]}`
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"no policy", nil, true},
		{"profiles off", map[string]string{"privacy-profiles.json": profiles("false")}, true},
		{"profiles on", map[string]string{"privacy-profiles.json": profiles("true")}, false},
		{"required but missing", map[string]string{"privacy-required": "x\n"}, false},
		{"broken policy", map[string]string{"privacy-profiles.json": "{"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for name, data := range tc.files {
				if err := os.WriteFile(filepath.Join(home, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := PromptTitles(privacy.NewRuntime(home)); got != tc.want {
				t.Fatalf("PromptTitles=%v want %v", got, tc.want)
			}
		})
	}
	if !PromptTitles(nil) {
		t.Fatal("nil runtime hides titles")
	}
}
