package geom

import (
	"math"

	"github.com/paulmach/orb"
)

// Intersects is turf's booleanIntersects (what winnow's ST_Intersects runs): true when any part
// of one geometry touches any part of the other, boundaries included.
func Intersects(a, b orb.Geometry) bool {
	if a == nil || b == nil {
		return false
	}
	pa, pb := flatten(a), flatten(b)
	if len(pb) == 0 {
		return false
	}
	ba, bb := Bounds(a), Bounds(b)
	if !ba.Intersects(bb) {
		return false
	}
	for _, x := range pa {
		for _, y := range pb {
			if !disjoint(x, y) {
				return true
			}
		}
	}
	return false
}

// flatten splits multi-geometries into points, lines and polygons.
func flatten(g orb.Geometry) []orb.Geometry {
	switch g := g.(type) {
	case orb.Point, orb.LineString, orb.Polygon:
		return []orb.Geometry{g}
	case orb.Ring:
		return []orb.Geometry{orb.Polygon{g}}
	case orb.MultiPoint:
		return geometries(g)
	case orb.MultiLineString:
		return geometries(g)
	case orb.MultiPolygon:
		return geometries(g)
	case orb.Bound:
		return []orb.Geometry{g.ToPolygon()}
	}
	return nil
}

func geometries[S ~[]T, T orb.Geometry](s S) []orb.Geometry {
	out := make([]orb.Geometry, len(s))
	for i, g := range s {
		out[i] = g
	}
	return out
}

func disjoint(a, b orb.Geometry) bool {
	switch a := a.(type) {
	case orb.Point:
		switch b := b.(type) {
		case orb.Point:
			return a != b
		case orb.LineString:
			return !onLine(b, a)
		case orb.Polygon:
			return !inPolygon(a, b)
		}
	case orb.LineString:
		switch b := b.(type) {
		case orb.Point:
			return !onLine(a, b)
		case orb.LineString:
			return !lineOnLine(a, b)
		case orb.Polygon:
			return !lineInPolygon(b, a)
		}
	case orb.Polygon:
		switch b := b.(type) {
		case orb.Point:
			return !inPolygon(b, a)
		case orb.LineString:
			return !lineInPolygon(a, b)
		case orb.Polygon:
			return !polyInPoly(b, a)
		}
	}
	return false
}

func onSegment(s, e, p orb.Point) bool {
	dxc, dyc := p[0]-s[0], p[1]-s[1]
	dxl, dyl := e[0]-s[0], e[1]-s[1]
	if dxc*dyl-dyc*dxl != 0 {
		return false
	}
	if math.Abs(dxl) >= math.Abs(dyl) {
		if dxl > 0 {
			return s[0] <= p[0] && p[0] <= e[0]
		}
		return e[0] <= p[0] && p[0] <= s[0]
	}
	if dyl > 0 {
		return s[1] <= p[1] && p[1] <= e[1]
	}
	return e[1] <= p[1] && p[1] <= s[1]
}

func onLine(l orb.LineString, p orb.Point) bool {
	for i := 0; i+1 < len(l); i++ {
		if onSegment(l[i], l[i+1], p) {
			return true
		}
	}
	return false
}

// ringPosition is point-in-polygon-hao for one ring: 1 inside, 0 on the boundary, -1 outside.
func ringPosition(p orb.Point, ring []orb.Point) int {
	inside := false
	n := len(ring)
	if n < 2 {
		return -1
	}
	x, y := p[0], p[1]
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		a, b := ring[j], ring[i]
		if onSegment(a, b, p) {
			return 0
		}
		if (b[1] > y) != (a[1] > y) && x < (a[0]-b[0])*(y-b[1])/(a[1]-b[1])+b[0] {
			inside = !inside
		}
	}
	if inside {
		return 1
	}
	return -1
}

// inPolygon: inside the outer ring and not inside a hole; boundaries count as inside.
func inPolygon(p orb.Point, poly orb.Polygon) bool {
	if len(poly) == 0 {
		return false
	}
	switch ringPosition(p, poly[0]) {
	case -1:
		return false
	case 0:
		return true
	}
	for _, h := range poly[1:] {
		switch ringPosition(p, h) {
		case 0:
			return true
		case 1:
			return false
		}
	}
	return true
}

func segmentsCross(a1, a2, b1, b2 orb.Point) bool {
	denom := (b2[1]-b1[1])*(a2[0]-a1[0]) - (b2[0]-b1[0])*(a2[1]-a1[1])
	if denom == 0 {
		return false
	}
	ua := ((b2[0]-b1[0])*(a1[1]-b1[1]) - (b2[1]-b1[1])*(a1[0]-b1[0])) / denom
	ub := ((a2[0]-a1[0])*(a1[1]-b1[1]) - (a2[1]-a1[1])*(a1[0]-b1[0])) / denom
	return ua >= 0 && ua <= 1 && ub >= 0 && ub <= 1
}

func linesCross(a, b [][]orb.Point) bool {
	for _, la := range a {
		for i := 0; i+1 < len(la); i++ {
			sa := orb.Bound{Min: la[i], Max: la[i]}.Extend(la[i+1])
			for _, lb := range b {
				for j := 0; j+1 < len(lb); j++ {
					if !sa.Intersects(orb.Bound{Min: lb[j], Max: lb[j]}.Extend(lb[j+1])) {
						continue
					}
					if segmentsCross(la[i], la[i+1], lb[j], lb[j+1]) {
						return true
					}
				}
			}
		}
	}
	return false
}

// parts is a polygon's rings or a multilinestring's lines as plain point runs.
func parts[S ~[]T, T ~[]orb.Point](s S) [][]orb.Point {
	out := make([][]orb.Point, len(s))
	for i, r := range s {
		out[i] = r
	}
	return out
}

func lineOnLine(a, b orb.LineString) bool {
	if linesCross([][]orb.Point{a}, [][]orb.Point{b}) {
		return true
	}
	for _, p := range a {
		if onLine(b, p) {
			return true
		}
	}
	for _, p := range b {
		if onLine(a, p) {
			return true
		}
	}
	return false
}

func lineInPolygon(poly orb.Polygon, l orb.LineString) bool {
	for _, p := range l {
		if inPolygon(p, poly) {
			return true
		}
	}
	return linesCross([][]orb.Point{l}, parts(poly))
}

func polyInPoly(a, b orb.Polygon) bool {
	if len(a) > 0 {
		for _, p := range a[0] {
			if inPolygon(p, b) {
				return true
			}
		}
	}
	if len(b) > 0 {
		for _, p := range b[0] {
			if inPolygon(p, a) {
				return true
			}
		}
	}
	return linesCross(parts(a), parts(b))
}
