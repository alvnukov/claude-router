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
func runServiceLabels(args []string, out io.Writer) error {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("service-labels", flag.ContinueOnError)
	flags.SetOutput(out)
	dir := flags.String("home", env("ROUTER_HOME", filepath.Join(home, ".claude/local-router")), "router home")
	if err := flags.Parse(args); err != nil {
		return err
	}
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
	return json.NewEncoder(out).Encode(map[string]string{
		"blue": effectiveLabel(file.LabelPrefix, "blue"), "green": effectiveLabel(file.LabelPrefix, "green"),
		"caddy": effectiveLabel(file.LabelPrefix, "caddy"), "prefix": effectiveLabel(file.LabelPrefix, ""),
	})
}
