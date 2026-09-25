package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// runServiceLabels prints names derived from deploy.json. Shell commands and
// the disposable checker use this rather than keeping another label list.
// With -prefix it names a prefix that no deploy.json records yet, so the
// checker can inspect launchd before its cutover writes one.
func runServiceLabels(args []string, out io.Writer) error {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("service-labels", flag.ContinueOnError)
	flags.SetOutput(out)
	dir := flags.String("home", env("ROUTER_HOME", filepath.Join(home, ".claude/local-router")), "router home")
	proposed := flags.String("prefix", "", "name this label prefix instead of the one in deploy.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	prefix := *proposed
	if prefix != "" {
		if err := validateLabelPrefix(prefix); err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(filepath.Join(*dir, "deploy.json"))
		if err != nil {
			return err
		}
		var file deployFile
		if err := json.Unmarshal(data, &file); err != nil {
			return err
		}
		if err := file.validate(); err != nil {
			return fmt.Errorf("deploy.json: %w", err)
		}
		prefix = file.LabelPrefix
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"blue": effectiveLabel(prefix, "blue"), "green": effectiveLabel(prefix, "green"),
		"caddy": effectiveLabel(prefix, "caddy"), "prefix": effectiveLabel(prefix, ""),
		"default_prefix": effectiveLabel(prefix, "") == defaultRouterLabel,
	})
}
