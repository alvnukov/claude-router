package main

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"

	conf "localrouter/internal/config"
)

var codexStores = struct {
	sync.Mutex
	m map[string]*codexAuthStore
}{m: map[string]*codexAuthStore{}}

// codexStoreFor gives each Codex connection its own credential slot. The
// pre-existing "codex" without an id keeps the legacy file; any other slot
// lives beside it and is named by both provider name and id, so a recreated
// name never reads an old token.
func codexStoreFor(p provider) (*codexAuthStore, error) {
	if p.Type != "codex" {
		return nil, fmt.Errorf("provider %q: не Codex", p.Name)
	}
	if p.AuthID == "" {
		if p.Name == "codex" || p.Name == "" {
			return codexAuth, nil
		}
		return nil, fmt.Errorf("provider %q: у подключения Codex нет auth_id", p.Name)
	}
	if !conf.AuthIDOK(p.AuthID) {
		return nil, fmt.Errorf("provider %q: неверный auth_id", p.Name)
	}
	path := filepath.Join(filepath.Dir(codexAuth.path), "codex-auth-"+hex.EncodeToString([]byte(p.Name))+"-"+p.AuthID+".json")
	codexStores.Lock()
	defer codexStores.Unlock()
	if s, ok := codexStores.m[path]; ok {
		return s, nil
	}
	s := &codexAuthStore{path: path, cliPath: codexAuth.cliPath, issuer: codexAuth.issuer, client: codexAuth.client, life: codexAuth.life}
	codexStores.m[path] = s
	return s, nil
}
