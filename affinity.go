package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"localrouter/internal/history"
)

// Affinity is scoped to a session, incoming model/effort and the current pool
// name. Only removing or changing the pinned member invalidates its binding.
type sessionBinding struct {
	Model     string
	Selection string
	Pool      string // pool name, for the per-pool session count
	Provider  string // connection of Model
	Used      time.Time
}

// poolRoute names the pool a request was routed to and its type. The zero
// value (a model route, or a caller that does not route) never balances.
type poolRoute struct{ Name, Type string }

// balanceCriterion scores a connection for a new session of a balance pool.
// The lowest score wins; a tie keeps pool order. It runs under h.mu.
type balanceCriterion func(h *health, pool string, p provider) int

// sessionsOnConnection counts the live sessions of this pool bound to p.
func sessionsOnConnection(h *health, pool string, p provider) int {
	n := 0
	for _, b := range h.sessions {
		if b.Pool == pool && b.Provider == p.Name {
			n++
		}
	}
	return n
}

// balanceFirst moves the first member of the best-scored connection to the
// front. Connections are compared, not models: each is represented by its
// first member that is not cooling, and a cooling member never leads.
func (h *health) balanceFirst(pool string, candidates []candidate) []candidate {
	score := h.balanceBy
	if score == nil {
		score = sessionsOnConnection
	}
	best, bestScore := -1, 0
	seen := map[string]bool{}
	for i, c := range candidates {
		if c.Stat.Cooling() || seen[c.Provider.Name] {
			continue
		}
		seen[c.Provider.Name] = true
		if s := score(h, pool, c.Provider); best < 0 || s < bestScore {
			best, bestScore = i, s
		}
	}
	if best <= 0 {
		return candidates
	}
	lead := candidates[best]
	copy(candidates[1:best+1], candidates[:best])
	candidates[0] = lead
	return candidates
}

func affinityKey(cfg config, body []byte, req anthropicRequest) string {
	session := history.SessionOf(body)
	if session == "" {
		return ""
	}
	effort := req.OutputConfig.Effort
	if effort == "" {
		effort = "default"
	}
	definition, _ := json.Marshal([]string{session, req.Model, effort, cfg.Local.ActiveProfile, cfg.RouteFor(req.Model, effort).Pool})
	digest := sha256.Sum256(definition)
	return hex.EncodeToString(digest[:])
}

// bindCandidates atomically chooses the first member for a session. A bound
// session keeps its member; in a balance pool (poolRoute) a new session goes
// to the least-loaded connection, chosen under the same lock that records it.
func (h *health) bindCandidates(scope string, pool poolRoute, candidates []candidate) []candidate {
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
				h.sessions[scope] = sessionBinding{Model: cand.Key, Selection: candidateSelection(cand), Pool: pool.Name, Provider: cand.Provider.Name, Used: now}
				return candidates
			}
		}
		// The bound member is gone: the stale binding must not count for its
		// old connection.
		delete(h.sessions, scope)
	}
	if pool.Type == PoolBalance {
		candidates = h.balanceFirst(pool.Name, candidates)
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
	h.sessions[scope] = sessionBinding{Model: candidates[0].Key, Selection: candidateSelection(candidates[0]), Pool: pool.Name, Provider: candidates[0].Provider.Name, Used: now}
	return candidates
}

func (h *health) moveSession(scope, from string, to candidate) {
	if scope == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if binding, ok := h.sessions[scope]; ok && binding.Model == from {
		h.sessions[scope] = sessionBinding{Model: to.Key, Selection: candidateSelection(to), Pool: binding.Pool, Provider: to.Provider.Name, Used: time.Now()}
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
