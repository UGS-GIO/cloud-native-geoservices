package conformance

import (
	"slices"
	"testing"
)

func rows(ids ...float64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = map[string]any{"attributes": map[string]any{"feature_id": id}}
	}
	return out
}

func TestCompareDuplicateRows(t *testing.T) {
	ref := Answer{Tree: map[string]any{"features": rows(1, 2)}}
	ours := Answer{Tree: map[string]any{"features": rows(1, 2, 2)}}
	for _, ordered := range []bool{false, true} {
		diffs := Compare(ref, ours, ordered)
		if !slices.ContainsFunc(diffs, func(d Diff) bool { return d.String() == "features (length)" }) {
			t.Errorf("ordered=%v: %v, want a length diff", ordered, diffs)
		}
	}
}
