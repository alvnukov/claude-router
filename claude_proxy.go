package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"localrouter/internal/platform"
)

const claudeBaseURLKey = "ANTHROPIC_BASE_URL"

type claudeProxy struct {
	mu   sync.Mutex
	path string
}

type claudeProxyBackup struct {
	Target      string          `json:"target"`
	Previous    json.RawMessage `json:"previous,omitempty"`
	EnvExisted  bool            `json:"env_existed"`
	EnvNull     bool            `json:"env_null"`
	FileExisted bool            `json:"file_existed"`
}

type claudeProxyView struct {
	Enabled, CanRestore bool
	Error, Flash        string
}

func newClaudeProxy() *claudeProxy {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return &claudeProxy{}
		}
		dir = filepath.Join(home, ".claude")
	}
	return &claudeProxy{path: filepath.Join(dir, "settings.json")}
}

// clientListen is the address clients should use to reach the router.
func (c config) clientListen() string {
	if c.publicListen != "" {
		return c.publicListen
	}
	return c.listen
}

func routerClientURL(listen string) (string, error) {
	if listen == "" {
		listen = "127.0.0.1:8787"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("не удалось определить адрес роутера")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func (p *claudeProxy) settingsPath() (string, error) {
	if p.path == "" {
		return "", fmt.Errorf("не удалось найти настройки Claude")
	}
	// Keep an existing symlink intact, changing its target atomically.
	resolved, err := filepath.EvalSymlinks(p.path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := os.Lstat(p.path); err == nil {
		return "", fmt.Errorf("ссылка на настройки Claude ведёт в отсутствующий файл")
	}
	return p.path, nil
}

func readClaudeSettings(path string) (map[string]json.RawMessage, map[string]json.RawMessage, []byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var root map[string]json.RawMessage
	if err = json.Unmarshal(data, &root); err != nil || root == nil {
		return nil, nil, nil, fmt.Errorf("настройки Claude содержат некорректный JSON; файл не изменён")
	}
	env := map[string]json.RawMessage{}
	if raw, ok := root["env"]; ok && string(raw) != "null" {
		if err = json.Unmarshal(raw, &env); err != nil || env == nil {
			return nil, nil, nil, fmt.Errorf("env в настройках Claude должен быть объектом")
		}
	}
	return root, env, data, nil
}

func readClaudeProxyBackup(path string) (*claudeProxyBackup, error) {
	data, err := os.ReadFile(path + ".router-proxy-backup")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var backup claudeProxyBackup
	if err = json.Unmarshal(data, &backup); err != nil || backup.Target == "" {
		return nil, fmt.Errorf("не удалось прочитать сохранённые настройки подключения Claude")
	}
	return &backup, nil
}

func proxyURL(env map[string]json.RawMessage) string {
	var value string
	_ = json.Unmarshal(env[claudeBaseURLKey], &value) // absent or not a string: no proxy
	return strings.TrimRight(value, "/")
}

func (p *claudeProxy) view(target string) claudeProxyView {
	p.mu.Lock()
	defer p.mu.Unlock()
	path, err := p.settingsPath()
	if err != nil {
		return claudeProxyView{Error: err.Error()}
	}
	_, env, _, err := readClaudeSettings(path)
	if err != nil {
		return claudeProxyView{Error: err.Error()}
	}
	backup, err := readClaudeProxyBackup(path)
	if err != nil {
		return claudeProxyView{Error: err.Error()}
	}
	current := proxyURL(env)
	enabled := current == target || (backup != nil && current == backup.Target)
	return claudeProxyView{Enabled: enabled, CanRestore: enabled && backup != nil}
}

// Change only the endpoint key. Restoring merges into the current file so edits
// Claude or the user made while the proxy was enabled survive.
func (p *claudeProxy) set(target string, enable bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	path, err := p.settingsPath()
	if err != nil {
		return err
	}
	root, env, before, err := readClaudeSettings(path)
	if err != nil {
		return err
	}
	backup, err := readClaudeProxyBackup(path)
	if err != nil {
		return err
	}
	current := proxyURL(env)
	if enable {
		if current == target {
			return nil
		}
		// Reconfiguration of the router's port preserves the original endpoint.
		if backup == nil || current != backup.Target {
			raw, existed := root["env"]
			backup = &claudeProxyBackup{Target: target, Previous: env[claudeBaseURLKey], EnvExisted: existed, EnvNull: bytes.Equal(bytes.TrimSpace(raw), []byte("null")), FileExisted: before != nil}
		} else {
			backup.Target = target
		}
		data, err := json.MarshalIndent(backup, "", "  ")
		if err != nil {
			return err
		}
		if err = writePrivateAtomic(path+".router-proxy-backup", data); err != nil {
			return err
		}
		env[claudeBaseURLKey], _ = json.Marshal(target)
	} else {
		if current != target && (backup == nil || current != backup.Target) {
			return fmt.Errorf("адрес Claude уже изменён вне роутера; обновите страницу")
		}
		if backup != nil && backup.Previous != nil {
			env[claudeBaseURLKey] = backup.Previous
		} else {
			delete(env, claudeBaseURLKey)
		}
	}
	if !enable && len(env) == 0 && backup != nil && !backup.EnvExisted {
		delete(root, "env")
	} else if !enable && len(env) == 0 && backup != nil && backup.EnvNull {
		root["env"] = json.RawMessage("null")
	} else {
		root["env"], _ = json.Marshal(env)
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	// Detect edits since the read instead of silently overwriting them.
	latest, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !bytes.Equal(latest, before) {
		return fmt.Errorf("настройки Claude изменились во время сохранения; повторите действие")
	}
	if !enable && backup != nil && !backup.FileExisted && len(root) == 0 {
		err = os.Remove(path)
		if os.IsNotExist(err) {
			err = nil
		}
	} else {
		err = writePrivateAtomic(path, append(data, '\n'))
	}
	if err != nil {
		return err
	}
	if !enable {
		if err = os.Remove(path + ".router-proxy-backup"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writePrivateAtomic(path string, data []byte) error {
	if err := platform.MkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	return platform.WriteFileAtomic(path, data, 0o600)
}

func (u *uiServer) claudeProxyView() claudeProxyView {
	target, err := routerClientURL(u.cs.get().clientListen())
	if err != nil {
		return claudeProxyView{Error: err.Error()}
	}
	return u.claudeProxy.view(target)
}

func (u *uiServer) settingsClaudeProxy(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm() // a malformed form reads as empty fields
	op := r.FormValue("op")
	target, err := routerClientURL(u.cs.get().clientListen())
	if err == nil && op != "connect" && op != "restore" {
		err = fmt.Errorf("неизвестное действие")
	}
	if err == nil {
		err = u.claudeProxy.set(target, op == "connect")
	}
	v := u.claudeProxyView()
	if err != nil {
		v.Error = err.Error()
	} else if op == "connect" {
		v.Flash = "Claude подключён к роутеру. Перезапустите Claude Code."
	} else {
		v.Flash = "Прежние настройки подключения восстановлены. Перезапустите Claude Code."
	}
	u.render(w, "claude-proxy", v)
}
