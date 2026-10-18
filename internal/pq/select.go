package pq

import (
	"context"
	"slices"

	"golang.org/x/sync/errgroup"
)

// Page is a slice of the selected rows: offset, and at most limit of them (plus one more, which
// tells the caller there are more).
type Page struct {
	Offset, Limit int64
}

// Selection is what a plan asks of a file.
type Selection struct {
	IDs  []int64
	Box  *[4]float64 // lon/lat prefilter
	Page *Page
}

func overlaps(g Group, b *[4]float64) bool {
	return !(g.Xmax < b[0] || g.Xmin > b[2] || g.Ymax < b[1] || g.Ymin > b[3])
}

// Select returns the rows a selection picks, in file (= feature_id) order.
func (l *Layer) Select(ctx context.Context, s Selection) ([]int64, error) {
	var rows []int64
	switch {
	case len(s.IDs) > 0:
		if l.IDIsRow {
			seen := map[int64]bool{}
			for _, id := range s.IDs {
				r := id - 1
				if r >= 0 && r < l.Rows && !seen[r] {
					seen[r] = true
					rows = append(rows, r)
				}
			}
		} else {
			want := map[int64]bool{}
			for _, id := range s.IDs {
				want[id] = true
			}
			all := l.AllRows()
			got, err := l.Read(ctx, []string{"feature_id"}, all)
			if err != nil {
				return nil, err
			}
			for i, r := range all {
				if id, ok := got.Get(0, i).(int64); ok && want[id] {
					rows = append(rows, r)
				}
			}
		}
	case s.Box != nil && l.HasBBox:
		var err error
		if rows, err = l.inBox(ctx, s.Box); err != nil {
			return nil, err
		}
	case s.Page != nil:
		end := min(l.Rows, s.Page.Offset+s.Page.Limit+1) // one extra: is there more?
		for r := s.Page.Offset; r < end; r++ {
			rows = append(rows, r)
		}
	default:
		rows = l.AllRows()
	}
	if s.Box != nil && len(s.IDs) > 0 && l.HasBBox {
		// Both filters: keep the id rows the box prefilter keeps.
		inBox, err := l.Select(ctx, Selection{Box: s.Box})
		if err != nil {
			return nil, err
		}
		keep := map[int64]bool{}
		for _, r := range inBox {
			keep[r] = true
		}
		rows = slices.DeleteFunc(rows, func(r int64) bool { return !keep[r] })
	}
	slices.Sort(rows)
	// A page of a filtered selection (the plain page above is already one).
	if s.Page != nil && (s.Box != nil || len(s.IDs) > 0) {
		from := min(int64(len(rows)), s.Page.Offset)
		to := min(int64(len(rows)), s.Page.Offset+s.Page.Limit+1)
		rows = rows[from:to]
	}
	return rows, nil
}

// inBox is the rows whose bbox columns intersect box, from row groups whose statistics do. The
// covering columns are read whole per row group and scanned in place.
func (l *Layer) inBox(ctx context.Context, box *[4]float64) ([]int64, error) {
	var groups []int
	for g, gr := range l.Groups {
		if overlaps(gr, box) {
			groups = append(groups, g)
		}
	}
	if err := l.prefetch(ctx, l.bboxLeaves(), groups); err != nil {
		return nil, err
	}
	hits := make([][]int64, len(groups))
	eg, ectx := errgroup.WithContext(ctx)
	for k, g := range groups {
		eg.Go(func() error {
			var cols [4]*Values
			for c, name := range BBox {
				rs, err := l.runs(ectx, l.leaves[name], g, nil)
				if err != nil {
					return err
				}
				cols[c] = rs[0].v
			}
			start := l.Groups[g].Start
			for i := range int(l.Groups[g].Rows) {
				if at(cols[2], i) >= box[0] && at(cols[0], i) <= box[2] && at(cols[3], i) >= box[1] && at(cols[1], i) <= box[3] {
					hits[k] = append(hits[k], start+int64(i))
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return slices.Concat(hits...), nil
}

func (l *Layer) bboxLeaves() []leaf {
	out := make([]leaf, len(BBox))
	for i, name := range BBox {
		out[i] = l.leaves[name]
	}
	return out
}

// Warm fetches the chunks of cols in the row groups whose statistics meet box (every group when
// box is nil), so a read after the bbox scan finds them cached rather than waiting on them.
func (l *Layer) Warm(ctx context.Context, cols []string, box *[4]float64) error {
	var leaves []leaf
	for _, name := range cols {
		if lf, ok := l.leaves[name]; ok {
			leaves = append(leaves, lf)
		}
	}
	var groups []int
	for g, gr := range l.Groups {
		if box == nil || overlaps(gr, box) {
			groups = append(groups, g)
		}
	}
	return l.prefetch(ctx, leaves, groups)
}

// at is value i of a numeric column; a null bound compares as 0, as it does in JavaScript.
func at(v *Values, i int) float64 {
	if v.Null(i) {
		return 0
	}
	if v.Kind == KindFloat {
		return v.Floats[i]
	}
	return float64(v.Ints[i])
}
