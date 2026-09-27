package privacy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLabRoundtripIsEphemeralAndMetricsAreContentFree(t *testing.T) {
	home := t.TempDir()
	lab := NewLab(home)
	in := PreviewInput{Mode: "text", Input: "10.2.3.4\npassword=CANARY-PRIVATE-1234", Rules: json.RawMessage(`{}`), Enabled: true}
	r, err := lab.Preview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Roundtrip || r.Output == in.Input || strings.Contains(r.Output, "CANARY") {
		t.Fatal("mask/roundtrip failed")
	}
	back, err := lab.Restore(context.Background(), r.ID, r.Output)
	if err != nil || back.Output != in.Input {
		t.Fatal("restore", err)
	}
	metrics, _ := json.Marshal(lab.State())
	if strings.Contains(string(metrics), "CANARY") || strings.Contains(string(metrics), r.ID) {
		t.Fatal("metrics leaked contents/handle")
	}
	files, _ := os.ReadDir(home)
	if len(files) != 0 {
		t.Fatal("preview wrote files")
	}
	lab.Clear(r.ID)
	if _, err := lab.Restore(context.Background(), r.ID, r.Output); err == nil {
		t.Fatal("cleared handle remained usable")
	}
}

func TestLabBoundsLifetimeAndCapacity(t *testing.T) {
	lab := newLab(t.TempDir(), 20*time.Millisecond)
	in := PreviewInput{Mode: "text", Input: "synthetic", Rules: json.RawMessage(`{}`)}
	r, err := lab.Preview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for lab.State().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if lab.State().Active != 0 {
		t.Fatal("expired dictionary retained without another request")
	}
	if _, err := lab.Restore(context.Background(), r.ID, r.Output); err == nil || err.Error() != "expired" {
		t.Fatal("expired handle accepted", err)
	}
	lab = NewLab(t.TempDir())
	var ids []string
	defer func() {
		for _, id := range ids {
			lab.Clear(id)
		}
	}()
	for range 4 {
		r, err := lab.Preview(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	if _, err := lab.Preview(context.Background(), in); err == nil || err.Error() != "capacity" {
		t.Fatal("capacity ignored", err)
	}
	lab.Clear(ids[0])
	in.Input = strings.Repeat("x", PreviewLimit+1)
	if _, err := lab.Preview(context.Background(), in); err == nil || err.Error() != "too_large" {
		t.Fatal("input was truncated/accepted", err)
	}
}

func TestLabIsolatedFilterAndDisabledProfile(t *testing.T) {
	lab := NewLab(t.TempDir())
	in := PreviewInput{Mode: "text", Input: "10.2.3.4\npassword=synthetic-long-password", Rules: json.RawMessage(`{"filters":{"ipv4":false}}`)}
	r, err := lab.Preview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer lab.Clear(r.ID)
	if r.Output != in.Input || r.Enabled {
		t.Fatal("disabled profile did not bypass")
	}
	in.Filter = "ipv4"
	r, err = lab.Preview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer lab.Clear(r.ID)
	if strings.Contains(r.Output, "10.2.3.4") || !strings.Contains(r.Output, "synthetic-long-password") {
		t.Fatal("isolated filter combined policies")
	}
	back, err := lab.Restore(context.Background(), r.ID, r.Output)
	if err != nil || back.Output != in.Input {
		t.Fatal("isolated roundtrip", err)
	}
}
