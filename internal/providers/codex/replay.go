package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"localrouter/internal/providers"
)

// Replay belongs to one completed client response. It stores only native output
// and prefix fingerprints, not another copy of the client's entire prompt.
type Replay struct {
	Version     int                      `json:"version"`
	Scope       string                   `json:"scope"`
	Start       int                      `json:"start"`
	End         int                      `json:"end"`
	Prefix      string                   `json:"prefix"`
	Output      []json.RawMessage        `json:"output"`
	TurnState   string                   `json:"turn_state,omitempty"`
	Calls       int                      `json:"calls"`
	Usage       providers.ResponsesUsage `json:"usage"`
	UsageKnown  bool                     `json:"usage_known"`
	ResponseIDs []string                 `json:"response_ids,omitempty"`
}

// Capture encodes completed native state with a fingerprint of its client
// projection. It cannot match a later request that lacks that entire projection.
func Capture(scope string, payload []byte, result Completion) (json.RawMessage, error) {
	_, input, err := decodeRequest(payload)
	if err != nil {
		return nil, err
	}
	start := len(input)
	input = append(input, result.VisibleOutput()...)
	hashes, err := prefixHashes(input)
	if err != nil {
		return nil, err
	}
	state := Replay{Version: 1, Scope: scope, Start: start, End: len(input), Prefix: hashes[len(input)], Output: result.Output, TurnState: result.TurnState, Calls: result.Calls, Usage: result.Usage, UsageKnown: result.UsageKnown, ResponseIDs: result.ResponseIDs}
	b, err := json.Marshal(state)
	if len(b) > maxProtocolBytes {
		return nil, protocolError("replay_too_large")
	}
	return b, err
}

// Restore accepts records newest-first. A record is usable only when account,
// model and the *entire* visible prefix agree. Forks, compaction, edited messages
// and account changes cannot import unrelated encrypted model state.
func Restore(scope string, payload []byte, records []json.RawMessage) ([]byte, string, error) {
	if scope == "" || len(records) == 0 {
		return payload, "", nil
	}
	fields, input, err := decodeRequest(payload)
	if err != nil {
		return nil, "", err
	}
	hashes, err := prefixHashes(input)
	if err != nil {
		return nil, "", err
	}
	var matches []Replay
	starts := make(map[int]bool)
	for _, raw := range records {
		if len(raw) > maxProtocolBytes {
			continue
		}
		var state Replay
		if json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.Scope != scope || state.Start < 0 || state.End < state.Start || state.End > len(input) {
			continue
		}
		if starts[state.Start] || hashes[state.End] != state.Prefix {
			continue
		}
		starts[state.Start] = true
		matches = append(matches, state)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Start < matches[j].Start })
	var restored []json.RawMessage
	position, total := 0, 0
	turnState := ""
	for _, state := range matches {
		if state.Start < position {
			continue
		}
		restored = append(restored, input[position:state.Start]...)
		total += rawItemsSize(state.Output)
		if total > maxProtocolBytes {
			return nil, "", protocolError("replay_too_large")
		}
		restored = append(restored, state.Output...)
		position = state.End
		turnState = ""
		if state.End < len(input) && validHeader(state.TurnState) {
			var item wireItem
			if json.Unmarshal(input[state.End], &item) == nil && item.Type == "function_call_output" {
				turnState = state.TurnState
			}
		}
	}
	if len(matches) == 0 {
		return payload, "", nil
	}
	restored = append(restored, input[position:]...)
	for _, raw := range input[position:] {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "message" && item.Role == "user" {
			turnState = ""
		}
	}
	b, err := encodeRequest(fields, restored)
	return b, turnState, err
}

func prefixHashes(input []json.RawMessage) ([]string, error) {
	h := sha256.New()
	hashes := make([]string, 1, len(input)+1)
	hashes[0] = hex.EncodeToString(h.Sum(nil))
	for _, raw := range input {
		value, err := canonicalItem(raw)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(value)
		h.Write(sum[:])
		hashes = append(hashes, hex.EncodeToString(h.Sum(nil)))
	}
	return hashes, nil
}

func canonicalItem(raw json.RawMessage) ([]byte, error) {
	var item map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&item); err != nil {
		return nil, err
	}
	// Claude parses tool arguments and serializes them again. Object-key order
	// and insignificant whitespace must not discard otherwise identical state.
	if item["type"] == "function_call" {
		if text, ok := item["arguments"].(string); ok && json.Valid([]byte(text)) {
			var args any
			decoder := json.NewDecoder(bytes.NewBufferString(text))
			decoder.UseNumber()
			if decoder.Decode(&args) == nil {
				b, err := json.Marshal(args)
				if err != nil {
					return nil, err
				}
				item["arguments"] = string(b)
			}
		}
	}
	return json.Marshal(item)
}
