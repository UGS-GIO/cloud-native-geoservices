// Package geom turns WKB into geometries to draw at a scale, and does the geometry work Koop did:
// projection, Esri JSON shapes and the exact intersects test.
package geom

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/paulmach/orb"
)

// Shape is a geometry simplified for one scale. Speck is set when a polygon kept no ring at that
// tolerance and stands in as its bounding box, so the caller can thin overlapping specks.
type Shape struct {
	G     orb.Geometry
	Speck bool
}

// keep marks which of n points survive Douglas-Peucker at tolerance (degrees).
func keep(xs, ys []float64, tolerance float64) []bool {
	n := len(xs)
	kept := make([]bool, n)
	if n == 0 {
		return kept
	}
	kept[0], kept[n-1] = true, true
	if n <= 2 || tolerance <= 0 {
		for i := range kept {
			kept[i] = true
		}
		return kept
	}
	t2 := tolerance * tolerance
	stack := []int{0, n - 1}
	for len(stack) > 0 {
		a, b := stack[len(stack)-2], stack[len(stack)-1]
		stack = stack[:len(stack)-2]
		dx, dy := xs[b]-xs[a], ys[b]-ys[a]
		len2 := dx*dx + dy*dy
		worst, index := -1.0, -1
		for i := a + 1; i < b; i++ {
			t := 0.0
			if len2 != 0 {
				t = ((xs[i]-xs[a])*dx + (ys[i]-ys[a])*dy) / len2
			}
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
			ex, ey := xs[i]-(xs[a]+t*dx), ys[i]-(ys[a]+t*dy)
			if d2 := ex*ex + ey*ey; d2 > worst {
				worst, index = d2, i
			}
		}
		if worst > t2 {
			kept[index] = true
			stack = append(stack, a, index, index, b)
		}
	}
	return kept
}

type reader struct {
	b      []byte
	off    int
	order  binary.ByteOrder
	whole  bool // some polygon ring survived
	tol    float64
	xs, ys []float64 // scratch for one run of points
}

func (r *reader) need(n int) error {
	if r.off+n > len(r.b) {
		return fmt.Errorf("truncated WKB")
	}
	return nil
}

// header reads a geometry's byte order and type; EWKB flags and ISO 1000s give Z and M.
func (r *reader) header() (typ uint32, dims int, err error) {
	if err = r.need(5); err != nil {
		return
	}
	if r.b[r.off] == 1 {
		r.order = binary.LittleEndian
	} else {
		r.order = binary.BigEndian
	}
	typ = r.order.Uint32(r.b[r.off+1:])
	r.off += 5
	dims = 2
	if typ&0x80000000 != 0 {
		dims++
	}
	if typ&0x40000000 != 0 {
		dims++
	}
	if typ&0x20000000 != 0 {
		r.off += 4 // SRID
	}
	typ &= 0x0fffffff
	if typ > 1000 {
		if typ >= 3000 {
			dims += 2
		} else {
			dims++
		}
		typ %= 1000
	}
	return
}

func (r *reader) uint() (int, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := r.order.Uint32(r.b[r.off:])
	r.off += 4
	return int(v), nil
}

func (r *reader) point(dims int) (orb.Point, error) {
	if err := r.need(8 * dims); err != nil {
		return orb.Point{}, err
	}
	x := math.Float64frombits(r.order.Uint64(r.b[r.off:]))
	y := math.Float64frombits(r.order.Uint64(r.b[r.off+8:]))
	r.off += 8 * dims
	return orb.Point{x, y}, nil
}

// run reads n points and returns the ones Douglas-Peucker keeps; xs/ys hold all of them.
func (r *reader) run(dims int) ([]orb.Point, error) {
	n, err := r.uint()
	if err != nil {
		return nil, err
	}
	if err := r.need(8 * dims * n); err != nil {
		return nil, err
	}
	r.xs, r.ys = r.xs[:0], r.ys[:0]
	for i := 0; i < n; i++ {
		r.xs = append(r.xs, math.Float64frombits(r.order.Uint64(r.b[r.off:])))
		r.ys = append(r.ys, math.Float64frombits(r.order.Uint64(r.b[r.off+8:])))
		r.off += 8 * dims
	}
	kept := keep(r.xs, r.ys, r.tol)
	count := 0
	for _, k := range kept {
		if k {
			count++
		}
	}
	out := make([]orb.Point, 0, count)
	for i, k := range kept {
		if k {
			out = append(out, orb.Point{r.xs[i], r.ys[i]})
		}
	}
	return out, nil
}

func (r *reader) line(dims int) (orb.LineString, error) {
	out, err := r.run(dims)
	if err != nil || len(out) >= 2 || len(r.xs) == 0 {
		return out, err
	}
	n := len(r.xs)
	return orb.LineString{{r.xs[0], r.ys[0]}, {r.xs[n-1], r.ys[n-1]}}, nil
}

// speck is the bounding box of the last run, as a ring.
func (r *reader) speck() orb.Ring {
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for i := range r.xs {
		x0, x1 = min(x0, r.xs[i]), max(x1, r.xs[i])
		y0, y1 = min(y0, r.ys[i]), max(y1, r.ys[i])
	}
	return orb.Ring{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}
}

func (r *reader) polygon(dims int) (orb.Polygon, error) {
	count, err := r.uint()
	if err != nil {
		return nil, err
	}
	rings := orb.Polygon{}
	for i := 0; i < count; i++ {
		out, err := r.run(dims)
		if err != nil {
			return nil, err
		}
		if len(out) >= 4 {
			rings = append(rings, orb.Ring(out))
			r.whole = true
		} else if i == 0 {
			rings = append(rings, r.speck())
		}
	}
	return rings, nil
}

func (r *reader) geometry() (orb.Geometry, error) {
	typ, dims, err := r.header()
	if err != nil {
		return nil, err
	}
	switch typ {
	case 1:
		return r.point(dims)
	case 2:
		return r.line(dims)
	case 3:
		return r.polygon(dims)
	}
	n, err := r.uint()
	if err != nil {
		return nil, err
	}
	if n > len(r.b)-r.off {
		return nil, fmt.Errorf("truncated WKB") // every member takes at least a byte
	}
	switch typ {
	case 4:
		return members[orb.MultiPoint](r, n, dims, r.point)
	case 5:
		return members[orb.MultiLineString](r, n, dims, r.line)
	case 6:
		return members[orb.MultiPolygon](r, n, dims, r.polygon)
	}
	return nil, fmt.Errorf("unsupported WKB geometry type %d", typ)
}

// members reads the n member geometries of a multi geometry.
func members[S ~[]T, T any](r *reader, n, dims int, read func(int) (T, error)) (S, error) {
	out := make(S, 0, n)
	for range n {
		if _, _, err := r.header(); err != nil {
			return nil, err
		}
		m, err := read(dims)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// FromWKB reads WKB into a 2D geometry simplified to tolerance (degrees); ok is false for no
// geometry. Points the simplifier drops are never allocated.
func FromWKB(b []byte, tolerance float64) (Shape, bool, error) {
	if len(b) == 0 {
		return Shape{}, false, nil
	}
	r := &reader{b: b, tol: tolerance}
	g, err := r.geometry()
	if err != nil {
		return Shape{}, false, err
	}
	s := Shape{G: g}
	switch g.(type) {
	case orb.Polygon, orb.MultiPolygon:
		s.Speck = !r.whole
	}
	return s, true, nil
}
