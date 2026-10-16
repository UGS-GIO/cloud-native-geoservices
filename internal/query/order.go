package query

import (
	"math"
	"slices"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
)

// sorted orders feature indexes by orderByFields, as alasql's ORDER BY compares: a falsy value
// counts as the empty string, and values compare with JavaScript's loose < and ==. The sort is stable.
func (w *winnow) sorted(idx []int) []int {
	if len(w.order) == 0 {
		return idx
	}
	out := slices.Clone(idx)
	slices.SortStableFunc(out, func(a, b int) int {
		for _, k := range w.order {
			var va, vb any = "", ""
			if k.prop >= 0 {
				va, vb = w.c.Features[a].Props[k.prop], w.c.Features[b].Props[k.prop]
			}
			if c := compareKeys(va, vb, k.desc); c != 0 {
				return c
			}
		}
		return 0
	})
	return out
}

// compareKeys is one key of alasql's comparator.
func compareKeys(a, b any, desc bool) int {
	a, b = orEmpty(a), orEmpty(b)
	if js.LooseEqual(a, b) {
		return 0
	}
	if desc {
		if js.Less(a, b) {
			return 1
		}
		return -1
	}
	if js.Less(b, a) {
		return 1
	}
	return -1
}

func orEmpty(v any) any {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if !x {
			return ""
		}
	case int64:
		if x == 0 {
			return ""
		}
		return float64(x)
	case float64:
		if x == 0 || math.IsNaN(x) {
			return ""
		}
	case string:
		return x
	}
	return v
}
