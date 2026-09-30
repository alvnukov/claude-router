package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// Command previews masking locally. It never creates or mutates session files.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return command(ctx, args, stdout, stderr, Options{})
}
func command(ctx context.Context, args []string, stdout, stderr io.Writer, opt Options) error {
	if len(args) == 0 || args[0] != "mask" {
		return errors.New("usage: privacy mask [-home dir] [-rules file] <file>")
	}
	fs := flag.NewFlagSet("privacy mask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", os.Getenv("ROUTER_HOME"), "router state directory")
	rulesPath := fs.String("rules", "", "privacy.json path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("privacy mask: exactly one input file is required")
	}
	if *home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*home = filepath.Join(userHome, ".claude", "local-router")
	}
	explicit := *rulesPath != ""
	if !explicit {
		*rulesPath = filepath.Join(*home, "privacy.json")
	}
	r, err := LoadRules(*rulesPath)
	if err != nil {
		if explicit || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		r, err = ParseRules([]byte(`{}`))
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, "privacy.json отсутствует: встроенные детекторы и автозаписи; правила организации не заданы")
	}
	body, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	opt.ephemeral = true
	e, err := Open(*home, r, opt)
	if err != nil {
		return err
	}
	plain := true
	if n, err := scanJSON(body); err == nil && n.kind == '{' {
		for _, name := range []string{"system", "messages", "tools"} {
			if n.get(name) != nil {
				plain = false
			}
		}
	}
	request := body
	if plain {
		request, err = json.Marshal(struct {
			System string `json:"system"`
		}{string(body)})
		if err != nil {
			return err
		}
	}
	out, req, err := e.Mask(request)
	if err != nil {
		fmt.Fprintln(stderr, "privacy: rejected")
		return err
	}
	defer req.Close()
	if plain {
		text, ok := lookupString(out, "system")
		if !ok {
			return errors.New("privacy: missing preview text")
		}
		out = []byte(text)
	}
	if n, err := stdout.Write(out); err != nil {
		return err
	} else if n != len(out) {
		return io.ErrShortWrite
	}
	stats := req.Stats()
	fmt.Fprintf(stderr, "privacy: scope=%s version=%d", stats.Scope, stats.Version)
	kinds := make([]Kind, 0, len(stats.Masked))
	for k := range stats.Masked {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(stderr, " %s=%d", kind, stats.Masked[kind])
	}
	fmt.Fprintln(stderr)
	return nil
}
