package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"reflect"
	"testing"
)

// recorder is a command table whose entries note how they were called.
type recorder struct {
	name string
	args []string
	err  error
}

func (r *recorder) table(names ...string) []Entry {
	var table []Entry
	for _, name := range names {
		table = append(table, Entry{Name: name, Run: func(_ context.Context, args []string, _, _ io.Writer) error {
			r.name, r.args = name, args
			return r.err
		}})
	}
	return table
}

func TestDispatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		err        error
		wantCode   int
		wantRan    string
		wantArgs   []string
		wantStderr string
	}{
		{"no arguments serve", nil, nil, 0, "serve", []string{}, ""},
		{"named command gets the rest", []string{"status", "-home", "/h"}, nil, 0, "status", []string{"-home", "/h"}, ""},
		{"unknown command", []string{"frobnicate"}, nil, 2, "", nil, "usage: localrouter {serve|status}\n"},
		{"failure", []string{"status"}, errors.New("slot or Caddy launchd agent missing"), 1, "status", []string{}, "slot or Caddy launchd agent missing\n"},
		{"usage error", []string{"status"}, UsageError("usage: localrouter status -home DIR"), 2, "status", []string{}, "usage: localrouter status -home DIR\n"},
		{"help", []string{"status", "-h"}, flag.ErrHelp, 0, "status", []string{"-h"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{err: tc.err}
			var stdout, stderr bytes.Buffer
			code := Dispatch(t.Context(), r.table("serve", "status"), tc.args, &stdout, &stderr)
			if code != tc.wantCode || r.name != tc.wantRan || stderr.String() != tc.wantStderr {
				t.Fatalf("Dispatch(%q) = %d, ran %q, stderr %q; want %d, %q, %q", tc.args, code, r.name, stderr.String(), tc.wantCode, tc.wantRan, tc.wantStderr)
			}
			if tc.wantRan != "" && !reflect.DeepEqual(r.args, tc.wantArgs) {
				t.Fatalf("%s got args %q; want %q", r.name, r.args, tc.wantArgs)
			}
		})
	}
}
