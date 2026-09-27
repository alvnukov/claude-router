package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const replayStoreTestScope = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func replayStoreTestRecord(t *testing.T, scope, id, opaque string) json.RawMessage {
	t.Helper()
	item, err := json.Marshal(map[string]any{"type": "reasoning", "id": id, "summary": []any{}, "encrypted_content": opaque})
	if err != nil {
		t.Fatal(err)
	}
	state := Replay{Version: 1, Scope: scope, Prefix: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Output: []json.RawMessage{item}, Calls: 1, ResponseIDs: []string{id}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReplayStoreSurvivesRestartNewestFirstAndStaysPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replay")
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if records, err := store.Load(ctx, replayStoreTestScope); err != nil || len(records) != 0 {
		t.Fatalf("empty store = %d records, %v", len(records), err)
	}
	first := replayStoreTestRecord(t, replayStoreTestScope, "first", "opaque+/first==")
	second := replayStoreTestRecord(t, replayStoreTestScope, "second", "opaque+/second==")
	for _, record := range []json.RawMessage{first, second} {
		if err := store.Save(ctx, replayStoreTestScope, record); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := restarted.Load(ctx, replayStoreTestScope)
	if err != nil || len(records) != 2 {
		t.Fatalf("restart = %d records, %v", len(records), err)
	}
	if !bytes.Equal(records[0], second) || !bytes.Equal(records[1], first) {
		t.Fatal("replay order or opaque JSON changed after restart")
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, replayStoreTestScope+".json"): 0o600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("private mode %s: %v, %v", path, info, err)
			}
		}
	}
}

func TestReplayStoreConcurrentIndependentInstancesDoNotLoseRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replay")
	const writers = 12
	var group sync.WaitGroup
	errorsCh := make(chan error, writers)
	for i := range writers {
		store, err := NewReplayStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		record := replayStoreTestRecord(t, replayStoreTestScope, fmt.Sprintf("response-%d", i), "opaque")
		group.Go(func() { errorsCh <- store.Save(context.Background(), replayStoreTestScope, record) })
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.Load(context.Background(), replayStoreTestScope)
	if err != nil || len(records) != writers {
		t.Fatalf("concurrent records = %d, want %d; %v", len(records), writers, err)
	}
	ids := make(map[string]bool)
	for _, record := range records {
		var state Replay
		if err := json.Unmarshal(record, &state); err != nil {
			t.Fatal(err)
		}
		ids[state.ResponseIDs[0]] = true
	}
	if len(ids) != writers {
		t.Fatal("concurrent save replaced a neighbouring response")
	}
}

func TestReplayStoreRejectsInvalidScopesAndRecords(t *testing.T) {
	store, err := NewReplayStore(filepath.Join(t.TempDir(), "replay"))
	if err != nil {
		t.Fatal(err)
	}
	good := replayStoreTestRecord(t, replayStoreTestScope, "r", "opaque")
	ctx := context.Background()
	for _, scope := range []string{"", "../outside", strings.Repeat("A", 64), strings.Repeat("z", 64), replayStoreTestScope + "/child"} {
		if _, err := store.Load(ctx, scope); err == nil {
			t.Errorf("Load accepted scope %q", scope)
		}
		if err := store.Save(ctx, scope, good); err == nil {
			t.Errorf("Save accepted scope %q", scope)
		}
	}
	for _, bad := range []json.RawMessage{
		nil, json.RawMessage("null"), json.RawMessage("{"), json.RawMessage(`{}`),
		json.RawMessage(strings.Replace(string(good), `"version":1`, `"version":2`, 1)),
		replayStoreTestRecord(t, strings.Repeat("b", 64), "r", "other account"),
		json.RawMessage(strings.Replace(string(good), `"start":0`, `"start":-1`, 1)),
		json.RawMessage(strings.Replace(string(good), `"prefix":"`, `"prefix":"bad`, 1)),
		append(append(json.RawMessage(nil), good...), []byte(`{}`)...),
	} {
		if err := store.Save(ctx, replayStoreTestScope, bad); err == nil {
			t.Errorf("accepted invalid replay: %.120s", bad)
		}
	}
	if records, err := store.Load(ctx, replayStoreTestScope); err != nil || len(records) != 0 {
		t.Fatalf("invalid save left state behind: %d, %v", len(records), err)
	}
}

func TestReplayStoreCorruptionIsNotSilentlyReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replay")
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, replayStoreTestScope+".json")
	good := replayStoreTestRecord(t, replayStoreTestScope, "r", "opaque")
	for _, broken := range []string{`{`, `{"version":99,"scope":"` + replayStoreTestScope + `","records":[]}`, `{"version":1,"scope":"` + strings.Repeat("b", 64) + `","records":[]}`, `{"version":1,"scope":"` + replayStoreTestScope + `","records":[null]}`} {
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background(), replayStoreTestScope); err == nil {
			t.Error("corrupt store accepted")
		}
		if err := store.Save(context.Background(), replayStoreTestScope, good); err == nil {
			t.Error("corrupt store silently replaced")
		}
		if actual, err := os.ReadFile(path); err != nil || string(actual) != broken {
			t.Fatal("failed save changed existing state")
		}
	}
}

func TestReplayStoreRejectsOversizedFileWithoutReadingItAll(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replay")
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, replayStoreTestScope+".json"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((64 << 20) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, err := store.Load(context.Background(), replayStoreTestScope); err == nil {
		t.Fatal("oversized store accepted")
	}
	if err := store.Save(context.Background(), replayStoreTestScope, replayStoreTestRecord(t, replayStoreTestScope, "r", "opaque")); err == nil {
		t.Fatal("oversized store silently reset")
	}
}

func TestReplayStoreRejectsSymlinkFilesAndReplacedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode or privileges")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "replay")
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.WriteFile(outside, []byte("private outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".json", ".lock"} {
		link := filepath.Join(dir, replayStoreTestScope+suffix)
		// A preceding failed Save may legitimately have created its lock file.
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(context.Background(), replayStoreTestScope, replayStoreTestRecord(t, replayStoreTestScope, "r", "opaque")); err == nil {
			t.Fatalf("symlink %s accepted", suffix)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"-moved", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReplayStore(dir); err == nil {
		t.Fatal("constructor accepted symlink root")
	}
	if _, err := store.Load(context.Background(), replayStoreTestScope); err == nil {
		t.Fatal("replaced directory accepted")
	}
	if actual, err := os.ReadFile(outside); err != nil || string(actual) != "private outside data" {
		t.Fatal("symlink target was modified")
	}
}

func TestReplayStoreRejectsUnsafeExistingUnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows access is governed by the owned parent directory's ACL")
	}
	dir := filepath.Join(t.TempDir(), "replay")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReplayStore(dir); err == nil {
		t.Fatal("world-readable replay directory accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := replayStoreTestRecord(t, replayStoreTestScope, "r", "opaque")
	if err := store.Save(context.Background(), replayStoreTestScope, state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, replayStoreTestScope+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), replayStoreTestScope); err == nil {
		t.Fatal("world-readable replay file accepted")
	}
	if err := store.Save(context.Background(), replayStoreTestScope, state); err == nil {
		t.Fatal("unsafe existing replay file silently replaced")
	}
}

func TestReplayStoreSessionOverflowDoesNotEvictOlderRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replay")
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Four valid 13 MiB native states fit the session cap; a fifth must fail.
	// Seed the valid existing snapshot directly to avoid repeatedly rewriting
	// hundreds of MiB just to reach the boundary under test.
	large := replayStoreTestRecord(t, replayStoreTestScope, "old", strings.Repeat("x", 13<<20))
	path := filepath.Join(dir, replayStoreTestScope+".json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	write := func(data []byte) {
		t.Helper()
		if _, err := file.Write(data); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	write([]byte(`{"version":1,"scope":"` + replayStoreTestScope + `","records":[`))
	for i := range 4 {
		if i > 0 {
			write([]byte(","))
		}
		write(large)
	}
	write([]byte("]}"))
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	newRecord := json.RawMessage(bytes.ReplaceAll(large, []byte(`"old"`), []byte(`"new"`)))
	if err := store.Save(context.Background(), replayStoreTestScope, newRecord); err == nil {
		t.Fatal("oversized session accepted by evicting existing protocol state")
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("overflow changed the durable snapshot")
	}
}

func TestReplayStoreCancelledOperationsDoNotWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replay")
	store, err := NewReplayStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Save(ctx, replayStoreTestScope, replayStoreTestRecord(t, replayStoreTestScope, "r", "opaque")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Save = %v", err)
	}
	if _, err := store.Load(ctx, replayStoreTestScope); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Load = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, replayStoreTestScope+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled save wrote state: %v", err)
	}
}

func TestReplayStoreKeepsAccountSessionScopesIndependent(t *testing.T) {
	store, err := NewReplayStore(filepath.Join(t.TempDir(), "replay"))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{replayStoreTestScope, strings.Repeat("b", 64)} {
		record := replayStoreTestRecord(t, scope, "same-response-id", "opaque for "+scope)
		if err := store.Save(context.Background(), scope, record); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []string{replayStoreTestScope, strings.Repeat("b", 64)} {
		records, err := store.Load(context.Background(), scope)
		if err != nil || len(records) != 1 {
			t.Fatalf("scope separation failed: %d records, %v", len(records), err)
		}
		var state Replay
		if err := json.Unmarshal(records[0], &state); err != nil || state.Scope != scope {
			t.Fatalf("cross-scope state returned: scope=%q, err=%v", state.Scope, err)
		}
	}
}
