package pq

import (
	"context"
	"math"
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
			// Read feature_id only in the row groups whose statistics may hold a wanted id.
			sorted := slices.Sorted(slices.Values(s.IDs))
			var cand []int64
			for _, g := range l.Groups {
				// The first id not below the group's minimum (all of them when it has none).
				i, _ := slices.BinarySearchFunc(sorted, g.IDMin, func(id int64, lo float64) int {
					if float64(id) < lo {
						return -1
					}
					return 1
				})
				if i == len(sorted) || float64(sorted[i]) > g.IDMax {
					continue
				}
				for r := g.Start; r < g.Start+g.Rows; r++ {
					cand = append(cand, r)
				}
			}
			if len(cand) == 0 {
				break
			}
			got, err := l.Read(ctx, []string{"feature_id"}, cand)
			if err != nil {
				return nil, err
			}
			for i, r := range cand {
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
	if s.Box != nil && len(s.IDs) > 0 && l.HasBBox && len(rows) > 0 {
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

// inBox is the rows whose bbox columns intersect box, from row groups whose statistics meet it
// and, where the file has a page index, pages whose bounds do.
func (l *Layer) inBox(ctx context.Context, box *[4]float64) ([]int64, error) {
	var groups []int
	var index [][2]int64
	for g, gr := range l.Groups {
		if !overlaps(gr, box) {
			continue
		}
		groups = append(groups, g)
		for _, lf := range l.bboxLeaves() {
			cc := l.chunkMeta(lf, g)
			if cc.ColumnIndexOffset > 0 && cc.OffsetIndexOffset > 0 {
				index = append(index, [2]int64{cc.ColumnIndexOffset, cc.ColumnIndexOffset + int64(cc.ColumnIndexLength)},
					[2]int64{cc.OffsetIndexOffset, cc.OffsetIndexOffset + int64(cc.OffsetIndexLength)})
			}
		}
	}
	// Usually inside the footer read already; else one request for every group's.
	if err := l.file.Prefetch(ctx, index); err != nil {
		return nil, err
	}
	var spans []rgSpan
	var sure [][][2]int64
	for _, g := range groups {
		segs, err := l.segments(ctx, g)
		if err != nil {
			return nil, err
		}
		if segs == nil {
			spans, sure = append(spans, rgSpan{g: g}), append(sure, nil)
			continue
		}
		// Rows of a segment that meets the box for sure are hits unread; the rest that may, read.
		rel, in := []int64{}, [][2]int64(nil)
		for _, sg := range segs {
			switch {
			case sg.within(box):
				in = append(in, [2]int64{sg.from, sg.to})
			case sg.meets(box):
				for i := sg.from; i < sg.to; i++ {
					rel = append(rel, i)
				}
			}
		}
		if len(in) == 0 && int64(len(rel)) == l.Groups[g].Rows {
			rel = nil
		}
		if rel == nil || len(rel) > 0 || len(in) > 0 {
			spans, sure = append(spans, rgSpan{g: g, rel: rel}), append(sure, in)
		}
	}
	if err := l.prefetch(ctx, l.bboxLeaves(), spans); err != nil {
		return nil, err
	}
	hits := make([][]int64, len(spans))
	eg, ectx := errgroup.WithContext(ctx)
	for k, sp := range spans {
		start := l.Groups[sp.g].Start
		for _, r := range sure[k] {
			for i := r[0]; i < r[1]; i++ {
				hits[k] = append(hits[k], start+i)
			}
		}
		if sp.rel != nil && len(sp.rel) == 0 {
			continue
		}
		eg.Go(func() error {
			var cols [4]cursor
			for c, name := range BBox {
				rs, err := l.runs(ectx, l.leaves[name], sp.g, sp.rel)
				if err != nil {
					return err
				}
				cols[c].runs = rs
			}
			test := func(i int64) {
				if cols[2].at(i) >= box[0] && cols[0].at(i) <= box[2] && cols[3].at(i) >= box[1] && cols[1].at(i) <= box[3] {
					hits[k] = append(hits[k], start+i)
				}
			}
			if sp.rel == nil {
				for i := range l.Groups[sp.g].Rows {
					test(i)
				}
			}
			for _, i := range sp.rel {
				test(i)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return slices.Concat(hits...), nil // Select sorts them
}

// segment is the rows [from, to) of a row group that lie in one page of each bbox column, and
// what the column index says of those pages, by column.
type segment struct {
	from, to int64
	lo, hi   [4]float64 // NaN where unknown
	nulls    [4]bool    // may hold a null
	values   [4]bool    // may hold a value
}

// segments splits row group g at the page boundaries of every bbox column; nil when a column
// has no page index.
func (l *Layer) segments(ctx context.Context, g int) ([]segment, error) {
	segs := []segment{{from: 0, to: l.Groups[g].Rows}}
	for c, lf := range l.bboxLeaves() {
		if l.chunkMeta(lf, g).OffsetIndexOffset == 0 {
			return nil, nil
		}
		ci, err := l.columnIndex(ctx, lf, g)
		if err != nil || ci == nil {
			return nil, err
		}
		lay, err := l.layout(ctx, lf, g)
		if err != nil {
			return nil, err
		}
		if n := len(lay.pages); len(ci.NullPages) != n || len(ci.MinValues) != n ||
			len(ci.MaxValues) != n || len(ci.NullCounts) != 0 && len(ci.NullCounts) != n {
			return nil, nil
		}
		t := l.chunkMeta(lf, g).MetaData.Type
		var out []segment
		for i, p := 0, 0; i < len(segs) && p < len(lay.pages); {
			sg, pg := segs[i], lay.pages[p]
			if from, to := max(sg.from, pg.first), min(sg.to, pg.first+pg.rows); from < to {
				sg.from, sg.to = from, to
				sg.lo[c], sg.hi[c] = number(t, ci.MinValues[p]), number(t, ci.MaxValues[p])
				sg.nulls[c] = ci.NullPages[p] || len(ci.NullCounts) == 0 || ci.NullCounts[p] > 0
				sg.values[c] = !ci.NullPages[p]
				out = append(out, sg)
			}
			if segs[i].to < pg.first+pg.rows {
				i++
			} else {
				p++
			}
		}
		segs = out
	}
	return segs, nil
}

// test is whether value v of bbox column c meets box: xmin and ymin reach down to the box's
// max, xmax and ymax up to its min.
func test(c int, v float64, box *[4]float64) bool {
	if c < 2 {
		return v <= box[c+2]
	}
	return v >= box[c-2]
}

// meets reports whether a row of s may meet box, a null counting as 0 as at reads it.
func (s *segment) meets(box *[4]float64) bool {
	for c := range 4 {
		bound := s.lo[c]
		if c >= 2 {
			bound = s.hi[c]
		}
		if !(s.nulls[c] && test(c, 0, box)) && !(s.values[c] && (math.IsNaN(bound) || test(c, bound, box))) {
			return false
		}
	}
	return true
}

// within reports whether every row of s meets box.
func (s *segment) within(box *[4]float64) bool {
	for c := range 4 {
		bound := s.hi[c]
		if c >= 2 {
			bound = s.lo[c]
		}
		if s.nulls[c] || !test(c, bound, box) {
			return false
		}
	}
	return true
}

// bound is a box holding every row's bbox, when no value is null or unknown.
func (s *segment) bound() ([4]float64, bool) {
	b := [4]float64{s.lo[0], s.lo[1], s.hi[2], s.hi[3]}
	for c := range 4 {
		if s.nulls[c] || math.IsNaN(b[c]) {
			return b, false
		}
	}
	return b, true
}

// Covered reports, for each of the sorted rows, whether the page index bounds its bbox by a box
// inside accepts, so its bbox need not be read.
func (l *Layer) Covered(ctx context.Context, rows []int64, inside func([4]float64) bool) ([]bool, error) {
	out := make([]bool, len(rows))
	spans, err := l.spans(rows)
	if err != nil {
		return nil, err
	}
	for _, sp := range spans {
		segs, err := l.segments(ctx, sp.g)
		if err != nil {
			return nil, err
		}
		k, last, ok := 0, -1, false
		for i, r := range sp.rel {
			for segs != nil && segs[k].to <= r {
				k++
			}
			if segs != nil && k != last {
				b, known := segs[k].bound()
				last, ok = k, known && inside(b)
			}
			out[sp.from+i] = ok
		}
	}
	return out, nil
}

// cursor reads a column's runs at increasing rows.
type cursor struct {
	runs []run
	k    int
}

func (c *cursor) at(row int64) float64 {
	for c.k+1 < len(c.runs) && c.runs[c.k+1].first <= row {
		c.k++
	}
	r := c.runs[c.k]
	return at(r.v, int(row-r.first))
}

func (l *Layer) bboxLeaves() []leaf {
	out := make([]leaf, len(BBox))
	for i, name := range BBox {
		out[i] = l.leaves[name]
	}
	return out
}

// Warm fetches what reading cols at the sorted rows needs, joined into a few requests, so the
// reads that follow find it cached.
func (l *Layer) Warm(ctx context.Context, cols []string, rows []int64) error {
	var leaves []leaf
	for _, name := range cols {
		if lf, ok := l.leaves[name]; ok {
			leaves = append(leaves, lf)
		}
	}
	spans, err := l.spans(rows)
	if err != nil || len(leaves) == 0 {
		return err
	}
	return l.prefetch(ctx, leaves, spans)
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
