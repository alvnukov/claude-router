package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Affinity is scoped to a session, incoming model/effort and the current pool
// name. Only removing or changing the pinned member invalidates its binding.
type sessionBinding struct {
	Model     string
	Selection string
	Used      time.Time
}

func affinityKey(cfg config, body []byte, req anthropicRequest) string {
	session := sessionOf(body)
	if session == "" {
		return ""
	}
	effort := req.OutputConfig.Effort
	if effort == "" {
		effort = "default"
	}
	definition, _ := json.Marshal([]string{session, req.Model, effort, cfg.local.ActiveProfile, cfg.routeFor(req.Model, effort).Pool})
	digest := sha256.Sum256(definition)
	return hex.EncodeToString(digest[:])
}

// bindCandidates atomically chooses a first model for new concurrent requests
// in the same session. Load balancing only applies before this first binding.
func (h *health) bindCandidates(scope string, candidates []candidate) []candidate {
	if scope == "" || len(candidates) == 0 {
		return candidates
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]sessionBinding{}
	}
	now := time.Now()
	for key, binding := range h.sessions {
		if now.Sub(binding.Used) > 24*time.Hour {
			delete(h.sessions, key)
		}
	}
	if binding, exists := h.sessions[scope]; exists {
		for i, cand := range candidates {
			if cand.Key == binding.Model && candidateSelection(cand) == binding.Selection {
				copy(candidates[1:i+1], candidates[:i])
				candidates[0] = cand
				h.sessions[scope] = sessionBinding{Model: cand.Key, Selection: candidateSelection(cand), Used: now}
				return candidates
			}
		}
	}
	if len(h.sessions) >= 4096 {
		oldestKey := ""
		oldest := now
		for key, binding := range h.sessions {
			if binding.Used.Before(oldest) {
				oldestKey, oldest = key, binding.Used
			}
		}
		delete(h.sessions, oldestKey)
	}
	h.sessions[scope] = sessionBinding{Model: candidates[0].Key, Selection: candidateSelection(candidates[0]), Used: now}
	return candidates
}

func (h *health) moveSession(scope, from string, to candidate) {
	if scope == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if binding, ok := h.sessions[scope]; ok && binding.Model == from {
		h.sessions[scope] = sessionBinding{Model: to.Key, Selection: candidateSelection(to), Used: time.Now()}
	}
}

func candidateSelection(c candidate) string {
	data, _ := json.Marshal(struct {
		Key      string
		Provider provider
		Efforts  map[string]string
	}{c.Key, c.Provider, c.Efforts})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (h *health) clearSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = nil
}
