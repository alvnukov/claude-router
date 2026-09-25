package main

import (
	"os"
	"path/filepath"
)

// History persists across restarts in ROUTER_UI_HISTORY_FILE (default
// history.jsonl next to the binary); internal/history keeps the file.

func historyPath() string {
	if v, ok := os.LookupEnv("ROUTER_UI_HISTORY_FILE"); ok {
		return v // "" disables
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "history.jsonl")
	}
	return "history.jsonl"
}
