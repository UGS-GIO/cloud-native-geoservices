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
	for _, g := range groups {
		rel, err := l.candidates(ctx, g, box)
		if err != nil {
			return nil, err
		}
		if rel == nil || len(rel) > 0 {
			spans = append(spans, rgSpan{g: g, rel: rel})
		}
	}
	if err := l.prefetch(ctx, l.bboxLeaves(), spans); err != nil {
		return nil, err
	}
	hits := make([][]int64, len(spans))
	eg, ectx := errgroup.WithContext(ctx)
	for k, sp := range spans {
		eg.Go(func() error {
			var cols [4]cursor
			for c, name := range BBox {
				rs, err := l.runs(ectx, l.leaves[name], sp.g, sp.rel)
				if err != nil {
					return err
				}
				cols[c].runs = rs
			}
			start := l.Groups[sp.g].Start
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
	return slices.Concat(hits...), nil
}

// candidates is the sorted rows of row group g in a page of every bbox column whose bounds could
// meet box, a null counting as 0 as at reads it; nil when the page index rules out no row.
func (l *Layer) candidates(ctx context.Context, g int, box *[4]float64) ([]int64, error) {
	rows := l.Groups[g].Rows
	keep := [][2]int64{{0, rows}}
	for c, lf := range l.bboxLeaves() {
		if l.chunkMeta(lf, g).OffsetIndexOffset == 0 {
			return nil, nil
		}
		ci, err := l.columnIndex(ctx, lf, g)
		if err != nil || ci == nil {
			return nil, err
		}
		lay, err := l.layout(ctx, lf, g)
		if err != nil || len(lay.pages) != len(ci.NullPages) {
			return nil, err
		}
		t := l.chunkMeta(lf, g).MetaData.Type
		var pass [][2]int64
		for p, pg := range lay.pages {
			// The xmin and ymin columns must reach down to the box's max; xmax and ymax up to its min.
			meets := func(v float64) bool { return v <= box[c+2] }
			bound := ci.MinValues[p]
			if c >= 2 {
				meets, bound = func(v float64) bool { return v >= box[c-2] }, ci.MaxValues[p]
			}
			nulls := ci.NullPages[p] || len(ci.NullCounts) == 0 || ci.NullCounts[p] > 0
			v := number(t, bound)
			if (nulls && meets(0)) || (!ci.NullPages[p] && (math.IsNaN(v) || meets(v))) {
				pass = append(pass, [2]int64{pg.first, pg.first + pg.rows})
			}
		}
		keep = intersect(keep, pass)
	}
	if len(keep) == 1 && keep[0] == [2]int64{0, rows} {
		return nil, nil
	}
	out := []int64{}
	for _, r := range keep {
		for i := r[0]; i < r[1]; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

// intersect is the rows in both sorted lists of [start, end) ranges.
func intersect(a, b [][2]int64) [][2]int64 {
	var out [][2]int64
	for i, j := 0, 0; i < len(a) && j < len(b); {
		if lo, hi := max(a[i][0], b[j][0]), min(a[i][1], b[j][1]); lo < hi {
			out = append(out, [2]int64{lo, hi})
		}
		if a[i][1] < b[j][1] {
			i++
		} else {
			j++
		}
	}
	return out
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
