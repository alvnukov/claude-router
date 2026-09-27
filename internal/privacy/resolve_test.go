package privacy

import (
	"slices"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name string
		in   []Span
		want []Span
	}{
		{"empty", nil, nil},
		{
			"longest wins",
			[]Span{{Start: 4, End: 8, Kind: KindHost}, {Start: 0, End: 12, Kind: KindEmail}},
			[]Span{{Start: 0, End: 12, Kind: KindEmail}},
		},
		{
			"equal length: first wins",
			[]Span{{Start: 3, End: 7, Kind: KindOrg}, {Start: 1, End: 5, Kind: KindPerson}},
			[]Span{{Start: 1, End: 5, Kind: KindPerson}},
		},
		{
			"touching spans both stay, sorted",
			[]Span{{Start: 5, End: 9, Kind: KindIPv4}, {Start: 0, End: 5, Kind: KindHost}},
			[]Span{{Start: 0, End: 5, Kind: KindHost}, {Start: 5, End: 9, Kind: KindIPv4}},
		},
		{
			"a long span evicts two short ones it covers",
			[]Span{{Start: 0, End: 3, Kind: KindOrg}, {Start: 2, End: 10, Kind: KindAddress}, {Start: 9, End: 11, Kind: KindOrg}, {Start: 11, End: 13, Kind: KindOrg}},
			[]Span{{Start: 2, End: 10, Kind: KindAddress}, {Start: 11, End: 13, Kind: KindOrg}},
		},
		{
			"duplicates collapse",
			[]Span{{Start: 1, End: 4, Kind: KindMAC}, {Start: 1, End: 4, Kind: KindMAC}},
			[]Span{{Start: 1, End: 4, Kind: KindMAC}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolve(c.in); !slices.Equal(got, c.want) {
				t.Fatalf("resolve = %+v, want %+v", got, c.want)
			}
		})
	}
}
