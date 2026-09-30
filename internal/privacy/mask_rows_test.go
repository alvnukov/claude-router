package privacy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"testing"
)

// Each row pins a session key sha256(uint64 key). The old PRF returned the
// value unchanged on these keys (zero mask on the free bits); rows under the
// threshold k < 8 must leave as a placeholder whatever the key.
func TestNetworkMaskRows(t *testing.T) {
	stub := regexp.MustCompile(`^<secret:[a-z0-9]+:[a-f0-9]{8}>$`)
	for _, row := range []struct {
		name  string
		key   uint64
		value string
		stub  bool
	}{
		{"cidr4 /16 in 10/8", 1, "10.8.0.0/16", false},
		{"cidr4 /24 in 192.168/16", 6, "192.168.77.0/24", false},
		{"ipv4 host", 398, "10.2.3.4", false},
		{"cidr6 /24 in fd00/8", 180, "fd12:3400::/24", false},
		{"mac", 2478, "02:42:ac:11:00:02", false},
		{"cidr4 /9 in 10/8 under threshold", 1, "10.128.0.0/9", true},
		{"cidr4 /15 in 10/8 under threshold", 1, "10.8.0.0/15", true},
		{"cidr4 /19 in 172.16/12 under threshold", 1, "172.16.32.0/19", true},
		{"cidr4 /23 in 192.168/16 under threshold", 1, "192.168.2.0/23", true},
		{"cidr6 /15 in fd00/8 under threshold", 1, "fd12::/15", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, err := ParseRules([]byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			e := fixedKeyEngine(t, r, fixedKey(row.key))
			body := requestBody("", "net "+row.value+" end")
			masked, req := mustMask(t, e, body)
			var out struct{ System string }
			if err := json.Unmarshal(masked, &out); err != nil {
				t.Fatal(err)
			}
			got := bytes.TrimSuffix(bytes.TrimPrefix([]byte(out.System), []byte("net ")), []byte(" end"))
			if string(got) == row.value {
				t.Fatalf("%s left unmasked", row.value)
			}
			if stub.Match(got) != row.stub {
				t.Fatalf("%s masked as %s, placeholder expected: %v", row.value, got, row.stub)
			}
			back, err := e.UnmaskJSON(req, masked)
			if err != nil || !bytes.Equal(back, body) {
				t.Fatalf("roundtrip changed (%v):\n%s\n%s", err, body, back)
			}
		})
	}
}
