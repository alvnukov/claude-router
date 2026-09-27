# Privacy corpus

Synthetic inputs for masking tests: network configs, compose files, Kubernetes
manifests, logs, mail, code and API bodies. Every value is made up. Addresses
come from documentation and private ranges (192.0.2.0/24, 198.51.100.0/24,
203.0.113.0/24, 2001:db8::/32, RFC 1918, fd00::/8, MAC 00:00:5e:00:53:xx),
names from reserved TLDs (.example, .test), and the organization is the
placeholder "Ромашка" / Romashka with its partner "Василёк" / Vasilek. People,
streets and phone numbers are invented as well. Secrets have the shape of real ones and are
fake: AWS documentation keys, tokens full of FAKE, keys that fail their checksums.

Layout:

- `src/<name>` holds the annotated source. Each value a masker must act on is
  written as `⟦kind:value⟧`.
- `corpus/<name>` is the same text with the marks removed. This is what a
  masker sees.
- `corpus/<name>.spans.json` gives the expected spans: byte offsets into the
  corpus file, the kind and the text.

`internal/privacy/corpus_test.go` checks that `corpus/` matches `src/`. After
editing `src/`, regenerate with:

    go test -run TestPrivacyCorpus -update-privacy-corpus ./internal/privacy

The corpus stays at the repository root so that `go run . privacy mask
testdata/privacy/corpus/<name>` works from there.

`rules.json` is the `privacy.json` the tests load: the corpus networks, the
organization domains, and dictionary entries for the organization, people,
addresses and single-label hosts. A single label such as `border1` carries no
domain suffix, so no pattern can tell it from an ordinary word; it is masked
only as a `host` entry in the dictionary.

Kinds:

| kind | expected |
|---|---|
| `ipv4`, `ipv6` | mask: address, including a link-local one that carries a MAC |
| `cidr4`, `cidr6` | mask: network with prefix length |
| `mac` | mask: in colon, dash or Cisco dotted form |
| `host` | mask: a DNS name or an internal single-label host |
| `email` | mask |
| `org` | mask: a word from the organization dictionary, in any grammatical case |
| `person` | a first name, surname or both, in any case; policy is left to the design (dictionary or a future detector) |
| `address` | a postal address or its part; policy is left to the design |
| `phone` | a phone number; policy is left to the design |
| `secret` | replace with a `<secret:kind:hash>` placeholder that lives for one request: key, token, password, private key, hash |
| `public` | well-known public infrastructure (8.8.8.8, github.com, api.anthropic.com); policy is left to the design, and tests choose |
| `keep` | a near miss that must stay as is: loopback, 0.0.0.0/0, netmasks, versions, OIDs, timestamps, UUIDs, digests, public keys |

Text outside any span must pass through unchanged. `keep` spans are marked so
that a test can assert them explicitly, and they are not exhaustive.

`golden/` freezes CLI output on synthetic fixtures with a fixed test-only key.
Inspect changes before explicitly regenerating:

    go test ./internal/privacy -run TestMaskCommandGolden -update-privacy-golden

Runtime-generated 10k credential cases, encoded-secret containers and hostile
responses live in `internal/privacy/*_test.go`; they are not production data.
