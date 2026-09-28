// Package uisession derives the session facts the UI shows: readable names from
// local Claude journals, a title from the first request, and the routes taken.
package uisession

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Identity is what a Claude journal says about a session. Only customTitle,
// cwd and gitBranch are decoded; message text is never read into it.
type Identity struct{ Title, Project, Branch string }

type nameEntry struct {
	identity Identity
	path     string
	stat     os.FileInfo
}

// Names caches session identities. The UI never waits for local Claude
// journals: Snapshot returns what is cached and refreshes in the background.
// Only metadata is cached; no transcript is retained or changed. Missing files
// are retried for new sessions. The zero value is ready to use.
type Names struct {
	mu      sync.Mutex
	root    string
	entries map[string]nameEntry
	next    time.Time
	loading bool
	wg      sync.WaitGroup
}

// Snapshot returns the cached identities of ids under the projects directory
// root and starts a background refresh when the cache is due.
func (c *Names) Snapshot(root string, ids []string) map[string]Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root != root {
		c.root, c.entries, c.next, c.loading = root, nil, time.Time{}, false
	}
	out := make(map[string]Identity, len(ids))
	for _, id := range ids {
		out[id] = c.entries[id].identity
	}
	if root != "" && !c.loading && time.Now().After(c.next) {
		c.loading = true
		previous := c.entries
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			entries := loadNames(root, ids, previous)
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.root == root {
				c.entries, c.loading, c.next = entries, false, time.Now().Add(10*time.Second)
			}
		}()
	}
	return out
}

// Wait blocks until background refreshes started so far have finished.
func (c *Names) Wait() { c.wg.Wait() }

func safeSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func loadNames(dir string, ids []string, previous map[string]nameEntry) map[string]nameEntry {
	out := make(map[string]nameEntry, len(ids))
	root, err := os.OpenRoot(dir)
	if err != nil {
		return out
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return out
	}
	projects, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return out
	}
	for _, id := range ids {
		if !safeSessionID(id) {
			continue
		}
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			name := filepath.Join(project.Name(), id+".jsonl")
			stat, err := root.Stat(name)
			if err != nil || !stat.Mode().IsRegular() {
				continue
			}
			old := previous[id]
			if old.path == name && old.stat != nil && os.SameFile(old.stat, stat) && old.stat.Size() == stat.Size() && old.stat.ModTime().Equal(stat.ModTime()) {
				out[id] = old
				break
			}
			f, err := root.Open(name)
			if err != nil {
				continue
			}
			identity := readIdentity(io.LimitReader(f, stat.Size()), id)
			f.Close()
			out[id] = nameEntry{identity: identity, path: name, stat: stat}
			break
		}
	}
	return out
}

func readIdentity(input io.Reader, id string) Identity {
	var out Identity
	reader := bufio.NewReader(input)
	var line []byte
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		// A large tool payload must not consume unbounded memory or hide a later
		// small rename record. Continue at the next line rather than stop scanning.
		if len(line)+len(fragment) > 1<<20 {
			oversized = true
			line = nil
		}
		if !oversized {
			line = append(line, fragment...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !oversized && len(line) > 0 {
			var record struct {
				Type        string `json:"type"`
				SessionID   string `json:"sessionId"`
				CustomTitle string `json:"customTitle"`
				CWD         string `json:"cwd"`
				GitBranch   string `json:"gitBranch"`
			}
			if json.Unmarshal(line, &record) == nil && record.SessionID == id {
				if record.Type == "custom-title" {
					out.Title = compact(record.CustomTitle, 120)
				}
				if record.CWD != "" {
					out.Project = compact(path.Base(strings.ReplaceAll(record.CWD, `\`, "/")), 80)
					out.Branch = compact(record.GitBranch, 100)
				}
			}
		}
		line = line[:0]
		oversized = false
		if err != nil {
			break
		}
	}
	return out
}

func compact(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
