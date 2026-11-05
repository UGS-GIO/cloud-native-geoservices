package geom

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/paulmach/orb"
)

// The box path of Prepared.Intersects answers as Intersects does, touches included: coordinates
// are snapped to a coarse grid half the time so edges, corners and collinear runs coincide.
func TestPreparedIntersectsBoxMatches(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	coord := func(snap bool) float64 {
		v := rng.Float64() * 10
		if snap {
			v = math.Round(v)
		}
		return v
	}
	pt := func(snap bool) orb.Point { return orb.Point{coord(snap), coord(snap)} }
	ring := func(snap bool, cx, cy, r float64) orb.Ring {
		n := 3 + rng.IntN(8)
		var out orb.Ring
		for i := range n {
			a := 2 * math.Pi * float64(i) / float64(n)
			d := r * (0.5 + rng.Float64()/2)
			p := orb.Point{cx + d*math.Cos(a), cy + d*math.Sin(a)}
			if snap {
				p = orb.Point{math.Round(p[0]), math.Round(p[1])}
			}
			out = append(out, p)
		}
		return append(out, out[0])
	}
	feature := func() orb.Geometry {
		snap := rng.IntN(2) == 0
		switch rng.IntN(6) {
		case 0:
			return pt(snap)
		case 1:
			return orb.MultiPoint{pt(snap), pt(snap)}
		case 2:
			l := orb.LineString{pt(snap)}
			for range 1 + rng.IntN(5) {
				l = append(l, pt(snap))
			}
			return l
		case 3:
			return orb.MultiLineString{{pt(snap), pt(snap)}, {pt(snap), pt(snap), pt(snap)}}
		case 4:
			cx, cy, r := coord(snap), coord(snap), 1+rng.Float64()*5
			p := orb.Polygon{ring(snap, cx, cy, r)}
			if rng.IntN(2) == 0 {
				p = append(p, ring(snap, cx, cy, r/3)) // a hole
			}
			return p
		}
		return orb.MultiPolygon{{ring(snap, coord(snap), coord(snap), 2)}, {ring(snap, coord(snap), coord(snap), 3)}}
	}
	n, hits := 0, 0
	for range 200000 {
		snap := rng.IntN(2) == 0
		a, b := pt(snap), pt(snap)
		box := orb.Bound{Min: orb.Point{min(a[0], b[0]), min(a[1], b[1])}, Max: orb.Point{max(a[0], b[0]), max(a[1], b[1])}}
		if box.Min[0] == box.Max[0] || box.Min[1] == box.Max[1] {
			continue
		}
		p := Prepare(box.ToPolygon())
		if !p.box {
			t.Fatal("not taken as a box")
		}
		g := feature()
		want, got := Intersects(p.G, g), p.Intersects(g)
		if got != want {
			t.Fatalf("box %v, %#v: got %v, want %v", box, g, got, want)
		}
		n++
		if want {
			hits++
		}
	}
	t.Logf("%d cases, %d intersecting", n, hits)
}

func BenchmarkIntersectsBox(b *testing.B) {
	// A dense polygon straddling the box's edge.
	var r orb.Ring
	for i := range 20000 {
		a := 2 * math.Pi * float64(i) / 20000
		r = append(r, orb.Point{5 + 4*math.Cos(a), 5 + 4*math.Sin(a)})
	}
	r = append(r, r[0])
	poly := orb.Polygon{r}
	box := orb.Bound{Min: orb.Point{8, 4}, Max: orb.Point{10, 6}}
	p := Prepare(box.ToPolygon())
	b.Run("general", func(b *testing.B) {
		for b.Loop() {
			Intersects(p.G, poly)
		}
	})
	b.Run("box", func(b *testing.B) {
		for b.Loop() {
			p.Intersects(poly)
		}
	})
}

// A closed ring's last-to-first segment has no length; it put every point sharing the first
// vertex's x on the boundary.
func TestClosingSegmentIsNotABoundary(t *testing.T) {
	box := orb.Bound{Min: orb.Point{3, 8}, Max: orb.Point{8, 9}}.ToPolygon()
	if inPolygon(orb.Point{3, 7}, box) || Intersects(box, orb.LineString{{3, 7}, {0, 9}}) {
		t.Error("a point below the box is on its boundary")
	}
	if !inPolygon(orb.Point{3, 8}, box) {
		t.Error("the corner is not on the boundary")
	}
}
