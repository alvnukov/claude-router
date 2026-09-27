package privacy

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// A tool can encode a secret it received in the preceding turn. The next
// request must not expose the reversible representation of that known secret.
func TestEncodedSecretAcrossTurns(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	secret := "FAKE-only-sensitive-value-984623"
	_, first := mustMask(t, e, requestBody("encoded", "password="+secret))
	defer first.Close()
	for _, encoded := range []string{base64.StdEncoding.EncodeToString([]byte(secret)), hex.EncodeToString([]byte(secret))} {
		body := requestBody("encoded", "tool output: "+encoded)
		masked, req, err := e.Mask(body)
		if err != nil {
			continue
		}
		req.Close()
		if bytes.Contains(masked, []byte(encoded)) {
			t.Error("reversibly encoded secret would reach the provider")
		}
	}
}
