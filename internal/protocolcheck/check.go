// Package protocolcheck detects unreviewed changes to the upstream Codex wire
// contract. A matching fingerprint is evidence of an unchanged audited source,
// not proof of server-side compatibility.
package protocolcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

type Manifest struct {
	Version    int        `json:"version"`
	Repository string     `json:"repository"`
	Revision   string     `json:"revision"`
	ReviewedOn string     `json:"reviewed_on"`
	Contracts  []Contract `json:"contracts"`
}

// Contract selects either a complete focused file, its production prefix, or a
// rustfmt-delimited declaration (including the attributes immediately above it).
// The deliberately conservative source comparison includes comments: a source
// refactor or formatting change requires review, never silently passes.
type Contract struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Anchor string `json:"anchor,omitempty"`
	End    string `json:"end,omitempty"`
	SHA256 string `json:"sha256"`
}

type Status string

const (
	Match   Status = "match"
	Drift   Status = "drift"
	Unknown Status = "unknown"
)

type Finding struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Status   Status `json:"status"`
	Expected string `json:"expected"`
	Actual   string `json:"actual,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Report struct {
	Status   Status    `json:"status"`
	Baseline string    `json:"baseline"`
	Findings []Finding `json:"findings"`
}

// Load rejects incomplete, ambiguous and future manifests rather than producing
// a green check with less coverage than the author intended.
func Load(r io.Reader) (Manifest, error) {
	var m Manifest
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, fmt.Errorf("decode manifest: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return m, fmt.Errorf("manifest must contain exactly one JSON object")
	}
	if err := m.validate(); err != nil {
		return m, err
	}
	return m, nil
}

func (m Manifest) validate() error {
	if m.Version != 1 || m.Repository != "https://github.com/openai/codex" || !validHex(m.Revision, 40) || m.ReviewedOn == "" || len(m.Contracts) == 0 {
		return fmt.Errorf("invalid manifest version, provenance, or empty contracts")
	}
	seen := make(map[string]bool, len(m.Contracts))
	for _, c := range m.Contracts {
		if c.ID == "" || seen[c.ID] || !fs.ValidPath(c.Path) || strings.Contains(c.Path, "\\") || !strings.HasPrefix(c.Path, "codex-rs/") || !validHex(c.SHA256, 64) {
			return fmt.Errorf("invalid or duplicate contract %q", c.ID)
		}
		seen[c.ID] = true
		switch c.Kind {
		case "file":
			if c.Anchor != "" || c.End != "" {
				return fmt.Errorf("file contract %q must not have anchors", c.ID)
			}
		case "prefix":
			if c.Anchor != "" || c.End == "" {
				return fmt.Errorf("prefix contract %q requires only an end marker", c.ID)
			}
		case "block":
			if c.Anchor == "" || c.End != "" {
				return fmt.Errorf("block contract %q requires only an anchor", c.ID)
			}
		case "range":
			if c.Anchor == "" || c.End == "" || c.Anchor == c.End {
				return fmt.Errorf("range contract %q requires distinct anchors", c.ID)
			}
		default:
			return fmt.Errorf("unknown selector %q in contract %q", c.Kind, c.ID)
		}
	}
	return nil
}

func validHex(s string, size int) bool {
	_, err := hex.DecodeString(s)
	return len(s) == size && err == nil
}

// Check only reads the supplied filesystem; it never fetches, invokes Codex,
// accesses credentials, edits a checkout, or updates the baseline.
func Check(source fs.FS, manifest Manifest) (Report, error) {
	report := Report{Status: Match, Baseline: manifest.Revision}
	if err := manifest.validate(); err != nil {
		report.Status = Unknown
		return report, err
	}
	for _, contract := range manifest.Contracts {
		finding := Finding{ID: contract.ID, Path: contract.Path, Status: Match, Expected: contract.SHA256}
		data, err := fs.ReadFile(source, contract.Path)
		if err == nil {
			finding.Actual, err = Fingerprint(data, contract)
		}
		if err != nil {
			finding.Status = Unknown
			finding.Error = err.Error()
			report.Status = Unknown
		} else if finding.Actual != finding.Expected {
			finding.Status = Drift
			if report.Status != Unknown {
				report.Status = Drift
			}
		}
		report.Findings = append(report.Findings, finding)
	}
	return report, nil
}

// Fingerprint includes wire attributes and all lines of the selected contract.
// Only CRLF is normalized so Windows checkouts compare identically.
func Fingerprint(data []byte, contract Contract) (string, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	start, end := 0, len(lines)
	var err error
	switch contract.Kind {
	case "file":
	case "prefix":
		end, err = uniqueLine(lines, contract.End)
	case "range":
		start, err = uniqueLine(lines, contract.Anchor)
		if err == nil {
			end, err = uniqueLine(lines, contract.End)
		}
	case "block":
		start, err = uniqueLine(lines, contract.Anchor)
		if err == nil {
			indent := lines[start][:len(lines[start])-len(strings.TrimLeft(lines[start], " \t"))]
			end = -1
			for i := start + 1; i < len(lines); i++ {
				if lines[i] == indent+"}" {
					end = i + 1
					break
				}
			}
			if end == -1 {
				err = fmt.Errorf("declaration %q has no rustfmt closing delimiter", contract.Anchor)
			}
			// Attributes can span multiple lines. Stop at a blank line or the
			// preceding declaration's closing delimiter, retaining serde tags.
			for start > 0 && strings.TrimSpace(lines[start-1]) != "" && strings.TrimSpace(lines[start-1]) != "}" {
				start--
			}
		}
	default:
		err = fmt.Errorf("unknown selector %q", contract.Kind)
	}
	if err != nil {
		return "", err
	}
	if end <= start {
		return "", fmt.Errorf("empty or reversed contract %q", contract.ID)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines[start:end], "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func uniqueLine(lines []string, marker string) (int, error) {
	index := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			if index != -1 {
				return -1, fmt.Errorf("ambiguous anchor %q", marker)
			}
			index = i
		}
	}
	if index == -1 {
		return -1, fmt.Errorf("missing anchor %q", marker)
	}
	return index, nil
}
