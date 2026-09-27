package main

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

type sessionIdentity struct{ Title, Project, Branch string }
type sessionNameEntry struct {
	identity sessionIdentity
	path     string
	stat     os.FileInfo
}

// The UI never waits for local Claude journals. Only metadata is cached; no
// transcript is retained or changed. Missing files are retried for new sessions.
type sessionNameCache struct {
	mu      sync.Mutex
	root    string
	entries map[string]sessionNameEntry
	next    time.Time
	loading bool
}

func (c *sessionNameCache) snapshot(root string, ids []string) map[string]sessionIdentity {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root != root {
		c.root, c.entries, c.next, c.loading = root, nil, time.Time{}, false
	}
	out := make(map[string]sessionIdentity, len(ids))
	for _, id := range ids {
		out[id] = c.entries[id].identity
	}
	if root != "" && !c.loading && time.Now().After(c.next) {
		c.loading = true
		previous := c.entries
		go func() {
			entries := loadSessionNames(root, ids, previous)
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.root == root {
				c.entries, c.loading, c.next = entries, false, time.Now().Add(10*time.Second)
			}
		}()
	}
	return out
}

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

func loadSessionNames(dir string, ids []string, previous map[string]sessionNameEntry) map[string]sessionNameEntry {
	out := make(map[string]sessionNameEntry, len(ids))
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
			identity := readSessionIdentity(io.LimitReader(f, stat.Size()), id)
			f.Close()
			out[id] = sessionNameEntry{identity: identity, path: name, stat: stat}
			break
		}
	}
	return out
}

func readSessionIdentity(input io.Reader, id string) sessionIdentity {
	var out sessionIdentity
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
					out.Title = compactSessionText(record.CustomTitle, 120)
				}
				if record.CWD != "" {
					out.Project = compactSessionText(path.Base(strings.ReplaceAll(record.CWD, `\`, "/")), 80)
					out.Branch = compactSessionText(record.GitBranch, 100)
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

func compactSessionText(text string, limit int) string {
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

func firstSessionPrompt(body []byte) string {
	var request struct {
		Messages []anthropicMsg `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		blocks, _ := decodeBlocks(message.Content)
		for _, block := range blocks {
			if block.Type != "text" {
				continue
			}
			text := strings.TrimSpace(block.Text)
			for {
				previous := text
				for _, tag := range []string{"system-reminder", "local-command-caveat", "local-command-stdout", "command-name", "command-message", "command-args", "ide_opened_file", "ide_selection"} {
					if strings.HasPrefix(text, "<"+tag+">") {
						_, rest, found := strings.Cut(text, "</"+tag+">")
						if !found {
							text = ""
						} else {
							text = strings.TrimSpace(rest)
						}
					}
				}
				if text == previous {
					break
				}
			}
			if title := compactSessionText(text, 120); title != "" {
				return title
			}
		}
	}
	return ""
}
