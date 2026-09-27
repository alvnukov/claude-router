// codex-protocol-check is an offline source drift sentinel, not an inference client.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"localrouter/internal/protocolcheck"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("codex-protocol-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("codex", "", "local Codex source directory (required)")
	baseline := flags.String("manifest", "internal/protocolcheck/baseline.json", "reviewed contract manifest")
	jsonOutput := flags.Bool("json", false, "emit a machine-readable report")
	if err := flags.Parse(args); err != nil || *source == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: codex-protocol-check -codex /path/to/codex [-manifest file] [-json]")
		return 2
	}
	file, err := os.Open(*baseline)
	if err != nil {
		fmt.Fprintf(stderr, "UNKNOWN: open baseline: %v\n", err)
		return 2
	}
	defer file.Close()
	manifest, err := protocolcheck.Load(file)
	if err != nil {
		fmt.Fprintf(stderr, "UNKNOWN: %v\n", err)
		return 2
	}
	// os.Root confines reads, including symlinks, to the requested checkout.
	root, err := os.OpenRoot(*source)
	if err != nil {
		fmt.Fprintf(stderr, "UNKNOWN: open Codex source: %v\n", err)
		return 2
	}
	defer root.Close()
	report, err := protocolcheck.Check(root.FS(), manifest)
	if err != nil {
		fmt.Fprintf(stderr, "UNKNOWN: %v\n", err)
		return 2
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "UNKNOWN: write report: %v\n", err)
			return 2
		}
	} else {
		fmt.Fprintf(stdout, "%s: %d contracts; reviewed Codex %s\n", report.Status, len(report.Findings), report.Baseline)
		for _, finding := range report.Findings {
			if finding.Status == protocolcheck.Match {
				continue
			}
			fmt.Fprintf(stdout, "  %s %s (%s)\n", finding.Status, finding.ID, finding.Path)
			if finding.Error != "" {
				fmt.Fprintf(stdout, "    %s\n", finding.Error)
			} else {
				fmt.Fprintf(stdout, "    expected %s\n    actual   %s\n", finding.Expected, finding.Actual)
			}
		}
		switch report.Status {
		case protocolcheck.Drift:
			fmt.Fprintln(stdout, "Review upstream changes and adapter regression tests before updating the baseline.")
		case protocolcheck.Match:
			fmt.Fprintln(stdout, "Audited source contracts match; live server compatibility is not checked.")
		}
	}
	switch report.Status {
	case protocolcheck.Match:
		return 0
	case protocolcheck.Drift:
		return 1
	default:
		return 2
	}
}
