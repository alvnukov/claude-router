package main

import (
	"encoding/json"
	"os"
	"time"

	"localrouter/internal/platform"
)

type routerSnapshot struct {
	Stats    map[string]*modelStat     `json:"stats"`
	Sessions map[string]sessionBinding `json:"sessions"`
}

func (h *health) saveSnapshot(path string) error {
	h.mu.Lock()
	state := routerSnapshot{Stats: make(map[string]*modelStat, len(h.m)), Sessions: make(map[string]sessionBinding, len(h.sessions))}
	for key, stat := range h.m {
		copy := *stat
		state.Stats[key] = &copy
	}
	for key, binding := range h.sessions {
		if time.Since(binding.Used) <= 24*time.Hour {
			state.Sessions[key] = binding
		}
	}
	h.mu.Unlock()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return platform.WriteFileAtomic(path, data, 0o600)
}

func (h *health) loadSnapshot(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state routerSnapshot
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Stats == nil {
		state.Stats = map[string]*modelStat{}
	}
	if state.Sessions == nil {
		state.Sessions = map[string]sessionBinding{}
	}
	for key, binding := range state.Sessions {
		if time.Since(binding.Used) > 24*time.Hour {
			delete(state.Sessions, key)
		}
	}
	h.mu.Lock()
	h.m = state.Stats
	h.sessions = state.Sessions
	h.mu.Unlock()
	return nil
}
