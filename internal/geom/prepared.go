package geom

import (
	"github.com/paulmach/orb"
	sf "github.com/peterstace/simplefeatures/geom"
)

// Prepared is a query geometry made ready to test many features against: its bounds, whether it
// is a plain box, and simplefeatures' prepared (indexed) form for the exact DE-9IM tests.
type Prepared struct {
	G     orb.Geometry
	Bound orb.Bound
	box   bool
	sf    *sf.PreparedGeometry
	calls int     // exact placements so far
	grid  []Place // gridN² cells over Bound, each placed exactly, once calls pass gridAfter
}

// Past gridAfter exact placements, a gridN² grid of placed cells answers most of the rest: a bbox
// whose cells all lie inside (or all outside) does too. It costs gridN² placements, once.
const gridN, gridAfter = 64, 4096

// Prepare readies g; the exact tests answer false if simplefeatures cannot take it.
func Prepare(g orb.Geometry) *Prepared {
	p := &Prepared{G: g, Bound: Bounds(g)}
	p.box = isBox(g, p.Bound)
	if pg, err := sf.Prepare(toSF(g)); err == nil {
		p.sf = &pg
	}
	return p
}

// Place is where a bbox lies against a prepared geometry.
type Place int8

const (
	Across  Place = iota // it may meet the boundary: only the feature itself can tell
	Outside              // disjoint from the geometry
	Inside               // in its interior, clear of the boundary
)

// Place locates bbox b, so a feature inside it is known to meet or miss the geometry unread.
func (p *Prepared) Place(b orb.Bound) Place {
	if !b.Intersects(p.Bound) {
		return Outside
	}
	if p.box {
		if p.clear(b) {
			return Inside
		}
		return Across
	}
	if p.sf == nil {
		return Across
	}
	if p.calls++; p.calls == gridAfter {
		p.fillGrid()
	}
	if p.grid != nil {
		if at := p.gridPlace(b); at != Across {
			return at
		}
	}
	return p.exact(b)
}

// clear reports b inside the bounds, off their edges.
func (p *Prepared) clear(b orb.Bound) bool {
	return b.Min[0] > p.Bound.Min[0] && b.Min[1] > p.Bound.Min[1] && b.Max[0] < p.Bound.Max[0] && b.Max[1] < p.Bound.Max[1]
}

// exact places b with simplefeatures.
func (p *Prepared) exact(b orb.Bound) Place {
	bb := toSF(boundShape(b))
	if in, err := p.sf.ContainsProperly(bb); err == nil && in {
		return Inside
	}
	if meets, err := p.sf.Intersects(bb); err == nil && !meets {
		return Outside
	}
	return Across
}

func (p *Prepared) fillGrid() {
	w, h := (p.Bound.Max[0]-p.Bound.Min[0])/gridN, (p.Bound.Max[1]-p.Bound.Min[1])/gridN
	if !(w > 0 && h > 0) {
		return
	}
	p.grid = make([]Place, gridN*gridN)
	for j := range gridN {
		for i := range gridN {
			x, y := p.Bound.Min[0]+float64(i)*w, p.Bound.Min[1]+float64(j)*h
			p.grid[j*gridN+i] = p.exact(orb.Bound{Min: orb.Point{x, y}, Max: orb.Point{x + w, y + h}})
		}
	}
}

// gridPlace is b's place when every cell it touches agrees, one cell of slack around it so
// rounding at a cell edge cannot leave a cell out.
func (p *Prepared) gridPlace(b orb.Bound) Place {
	cellOf := func(v, lo, hi float64) int {
		return min(max(int((v-lo)/(hi-lo)*gridN), 0), gridN-1)
	}
	i0, i1 := cellOf(b.Min[0], p.Bound.Min[0], p.Bound.Max[0]), cellOf(b.Max[0], p.Bound.Min[0], p.Bound.Max[0])
	j0, j1 := cellOf(b.Min[1], p.Bound.Min[1], p.Bound.Max[1]), cellOf(b.Max[1], p.Bound.Min[1], p.Bound.Max[1])
	first := p.grid[max(j0-1, 0)*gridN+max(i0-1, 0)]
	for j := max(j0-1, 0); j <= min(j1+1, gridN-1); j++ {
		for i := max(i0-1, 0); i <= min(i1+1, gridN-1); i++ {
			if p.grid[j*gridN+i] != first {
				return Across
			}
		}
	}
	if first == Inside && !p.clear(b) {
		return Across
	}
	return first
}

// Contains is the DE-9IM contains of the geometry over g.
func (p *Prepared) Contains(g orb.Geometry) bool {
	b := Bounds(g)
	if p.sf == nil || !p.Bound.Contains(b.Min) || !p.Bound.Contains(b.Max) {
		return false
	}
	ok, err := p.sf.Contains(toSF(g))
	return err == nil && ok
}

// Within is the DE-9IM within of the geometry in g.
func (p *Prepared) Within(g orb.Geometry) bool {
	b := Bounds(g)
	if p.sf == nil || !b.Contains(p.Bound.Min) || !b.Contains(p.Bound.Max) {
		return false
	}
	ok, err := p.sf.Within(toSF(g))
	return err == nil && ok
}

// boundShape is b as the simplest geometry covering it: a point, a segment or a polygon.
func boundShape(b orb.Bound) orb.Geometry {
	switch {
	case b.Min == b.Max:
		return b.Min
	case b.Min[0] == b.Max[0] || b.Min[1] == b.Max[1]:
		return orb.LineString{b.Min, b.Max}
	}
	return b.ToPolygon()
}

// isBox reports a polygon that is its bounds b: one closed ring through b's four corners along
// axis-parallel edges.
func isBox(g orb.Geometry, b orb.Bound) bool {
	p, ok := g.(orb.Polygon)
	if !ok || len(p) != 1 || len(p[0]) != 5 || p[0][0] != p[0][4] || b.Min[0] == b.Max[0] || b.Min[1] == b.Max[1] {
		return false
	}
	r := p[0]
	for i := range 4 {
		corner := (r[i][0] == b.Min[0] || r[i][0] == b.Max[0]) && (r[i][1] == b.Min[1] || r[i][1] == b.Max[1])
		if !corner || (r[i][0] != r[i+1][0]) == (r[i][1] != r[i+1][1]) || r[i] == r[(i+2)%4] {
			return false
		}
	}
	return true
}

// toSF is g as a simplefeatures geometry.
func toSF(g orb.Geometry) sf.Geometry {
	switch g := g.(type) {
	case orb.Point:
		return sf.NewPointXY(g[0], g[1]).AsGeometry()
	case orb.MultiPoint:
		return sf.NewMultiPointXY(flat(g)...).AsGeometry()
	case orb.LineString:
		return line(g).AsGeometry()
	case orb.MultiLineString:
		ls := make([]sf.LineString, len(g))
		for i, l := range g {
			ls[i] = line(l)
		}
		return sf.NewMultiLineString(ls).AsGeometry()
	case orb.Ring:
		return polygon(orb.Polygon{g}).AsGeometry()
	case orb.Polygon:
		return polygon(g).AsGeometry()
	case orb.MultiPolygon:
		ps := make([]sf.Polygon, len(g))
		for i, p := range g {
			ps[i] = polygon(p)
		}
		return sf.NewMultiPolygon(ps).AsGeometry()
	case orb.Collection:
		gs := make([]sf.Geometry, len(g))
		for i, x := range g {
			gs[i] = toSF(x)
		}
		return sf.NewGeometryCollection(gs).AsGeometry()
	}
	return sf.Geometry{}
}

func line(run []orb.Point) sf.LineString {
	return sf.NewLineString(sf.NewSequence(flat(run), sf.DimXY))
}

func polygon(p orb.Polygon) sf.Polygon {
	rings := make([]sf.LineString, len(p))
	for i, r := range p {
		rings[i] = line(r)
	}
	return sf.NewPolygon(rings)
}

func flat(run []orb.Point) []float64 {
	xy := make([]float64, 0, 2*len(run))
	for _, p := range run {
		xy = append(xy, p[0], p[1])
	}
	return xy
}

// Intersects is Intersects(p.G, g). Against a plain box (every map tile) it runs in one pass
// that usually stops early: a vertex of g in the box, else an edge of g across it, else the
// box's centre in g.
func (p *Prepared) Intersects(g orb.Geometry) bool {
	if !p.box {
		return Intersects(p.G, g)
	}
	switch g := g.(type) {
	case orb.Point:
		return p.Bound.Contains(g)
	case orb.MultiPoint:
		for _, q := range g {
			if p.Bound.Contains(q) {
				return true
			}
		}
		return false
	case orb.LineString:
		return p.runMeets(g)
	case orb.MultiLineString:
		for _, l := range g {
			if p.runMeets(l) {
				return true
			}
		}
		return false
	case orb.Polygon:
		return p.polygonMeets(g)
	case orb.MultiPolygon:
		for _, q := range g {
			if p.polygonMeets(q) {
				return true
			}
		}
		return false
	}
	return Intersects(p.G, g)
}

func (p *Prepared) polygonMeets(g orb.Polygon) bool {
	if len(g) == 0 {
		return false
	}
	for _, r := range g {
		if p.runMeets(r) {
			return true
		}
	}
	// No ring reaches the box: it lies wholly inside the polygon or wholly outside it.
	return inPolygon(p.Bound.Center(), g)
}

// runMeets reports a vertex of run in the box or a segment of it across the box.
func (p *Prepared) runMeets(run []orb.Point) bool {
	b := p.Bound
	for _, q := range run {
		if b.Contains(q) {
			return true
		}
	}
	for i := 0; i+1 < len(run); i++ {
		if segmentMeetsBox(run[i], run[i+1], b) {
			return true
		}
	}
	return false
}

// segmentMeetsBox reports segment ab touching box b, by separating axes: the box's axes (the
// bounds overlap) and the segment's normal (the box's corners are not all strictly on one side).
func segmentMeetsBox(a, c orb.Point, b orb.Bound) bool {
	if max(a[0], c[0]) < b.Min[0] || min(a[0], c[0]) > b.Max[0] || max(a[1], c[1]) < b.Min[1] || min(a[1], c[1]) > b.Max[1] {
		return false
	}
	dx, dy := c[0]-a[0], c[1]-a[1]
	side := func(x, y float64) float64 { return dx*(y-a[1]) - dy*(x-a[0]) }
	s0, s1 := side(b.Min[0], b.Min[1]), side(b.Max[0], b.Min[1])
	s2, s3 := side(b.Max[0], b.Max[1]), side(b.Min[0], b.Max[1])
	return !(s0 > 0 && s1 > 0 && s2 > 0 && s3 > 0) && !(s0 < 0 && s1 < 0 && s2 < 0 && s3 < 0)
}
