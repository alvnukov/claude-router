package privacy

// Kind names what a span holds. The names match the corpus marks.
type Kind string

const (
	KindIPv4    Kind = "ipv4"
	KindIPv6    Kind = "ipv6"
	KindCIDR4   Kind = "cidr4"
	KindCIDR6   Kind = "cidr6"
	KindMAC     Kind = "mac"
	KindHost    Kind = "host"
	KindEmail   Kind = "email"
	KindPhone   Kind = "phone"
	KindPerson  Kind = "person"
	KindLogin   Kind = "login"
	KindOrg     Kind = "org"
	KindUnit    Kind = "unit"
	KindProject Kind = "project"
	KindAddress Kind = "address"
	KindSecret  Kind = "secret"
	KindSource  Kind = "source"
)

// entryKinds are the kinds a privacy.json entry may name.
var entryKinds = []Kind{KindOrg, KindUnit, KindProject, KindPerson, KindAddress, KindHost, KindLogin, KindEmail, KindPhone}

// Span is one detected value: byte offsets into the text it was found in.
type Span struct {
	Start, End int
	Kind       Kind
	Value      string
}

// Detector finds values in a text. It is a pure function of the text.
type Detector interface {
	Detect(text string) []Span
}
