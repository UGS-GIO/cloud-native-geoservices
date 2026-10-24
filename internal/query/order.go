package query

import (
	"cmp"
	"slices"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/js"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/where"
)

// sorted orders feature indexes by orderByFields as SQL does: nulls first ascending and last
// descending, strings as where.CompareText orders them. The sort is stable.
func (w *winnow) sorted(idx []int) []int {
	if len(w.order) == 0 {
		return idx
	}
	out := slices.Clone(idx)
	slices.SortStableFunc(out, func(a, b int) int {
		for _, k := range w.order {
			var va, vb any
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

// asNumber is v when it is a number, with booleans as 0 and 1.
func asNumber(v any) (float64, bool) {
	if b, ok := v.(bool); ok {
		return js.Number(b), true
	}
	return js.AsNumber(v)
}

// compareKeys compares two values of one field, null lowest.
func compareKeys(a, b any, desc bool) int {
	c := 0
	switch {
	case a == nil && b == nil:
	case a == nil:
		c = -1
	case b == nil:
		c = 1
	default:
		x, xok := asNumber(a)
		y, yok := asNumber(b)
		sa, _ := a.(string)
		sb, _ := b.(string)
		if xok && yok {
			c = cmp.Compare(x, y)
		} else {
			c = where.CompareText(sa, sb)
		}
	}
	if desc {
		return -c
	}
	return c
}
