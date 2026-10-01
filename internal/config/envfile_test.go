package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	writeRaw(t, p, "# comment\nROUTER_LOCAL_MODEL=old\nOTHER=keep\n")
	err := writeEnv(p, map[string]string{"ROUTER_LOCAL_MODEL": "new", "ROUTER_CLOUD_ONLY": "a,b", "ROUTER_LOCAL_API_KEY": "k y"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "# comment\nROUTER_LOCAL_MODEL=new\nOTHER=keep\nROUTER_CLOUD_ONLY=a,b\nROUTER_LOCAL_API_KEY=\"k y\"\n"
	if string(got) != want {
		t.Fatalf("got:\n%s", got)
	}
	requirePrivateFile(t, p)
}

func TestReadEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	writeRaw(t, p, "# c\nexport A=1\nB=\"x y\"\nC='q'\nD=\nbad line\n")
	m, err := ReadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != "1" || m["B"] != "x y" || m["C"] != "q" || m["D"] != "" {
		t.Fatalf("%+v", m)
	}
	if _, ok := m["bad line"]; ok {
		t.Fatal("bad line parsed")
	}
}
