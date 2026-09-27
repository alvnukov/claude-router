package privacy

import (
	"cmp"
	"slices"
)

// resolve picks a non-overlapping subset of spans: the longest span wins an
// overlap, and between equal lengths the one that starts first. Spans that
// only touch both stay. The result is sorted by Start.
func resolve(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	order := slices.Clone(spans)
	slices.SortStableFunc(order, func(a, b Span) int {
		if c := cmp.Compare(b.End-b.Start, a.End-a.Start); c != 0 {
			return c
		}
		return cmp.Compare(a.Start, b.Start)
	})
	var kept []Span // sorted by Start
	for _, s := range order {
		i, _ := slices.BinarySearchFunc(kept, s.Start, func(k Span, start int) int {
			return cmp.Compare(k.Start, start)
		})
		if i > 0 && kept[i-1].End > s.Start {
			continue
		}
		if i < len(kept) && kept[i].Start < s.End {
			continue
		}
		kept = slices.Insert(kept, i, s)
	}
	return kept
}
