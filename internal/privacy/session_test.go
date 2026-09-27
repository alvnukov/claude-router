package privacy

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMapUnique(t *testing.T) {
	s := newSessionStore(t.TempDir(), time.Now)
	err := s.transaction("alpha", false, func(view *sessionView) error {
		if len(view.session.key) != 32 {
			t.Fatal("missing session key")
		}
		for range 1000 {
			view.session.add(mapRecord{Kind: KindPerson, Real: "alpha", Pseudo: "vuzerofel"})
		}
		if len(view.session.entries) != 1 {
			t.Fatal("duplicate real entries")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("torn-tail", func(t *testing.T) {
		p := filepath.Join(s.dir, "alpha.jsonl")
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.WriteString(`{"kind":"person","real":"truncated`); err != nil {
			t.Fatal(err)
		}
		f.Close()
		err = s.transaction("alpha", false, func(v *sessionView) error {
			v.session.add(mapRecord{Kind: KindPerson, Real: "beta", Pseudo: "zifobareg"})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		all, err := s.readAll()
		if err != nil || len(all["alpha"].entries) != 2 {
			t.Fatalf("torn tail: %v", err)
		}
	})
	t.Run("bad-key", func(t *testing.T) {
		p := filepath.Join(s.dir, "broken.jsonl")
		if err := os.WriteFile(p, []byte(`{"kind":"key","key":"`+hex.EncodeToString([]byte{1})+`"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.readAll(); err == nil {
			t.Fatal("short key accepted")
		}
	})
}
