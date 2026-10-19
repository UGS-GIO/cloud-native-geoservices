package geom

import (
	"math"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/project"
)

// Web Mercator on the sphere, computed step for step as proj4 does (what Koop projected with).
const (
	radius = 6378137.0
	d2r    = 0.01745329251994329577
	r2d    = 57.29577951308232088
	spi    = 3.14159265359
)

// Mercator wkids: what we project to and from.
var Mercator = map[int]bool{102100: true, 102113: true, 3857: true, 3785: true, 900913: true}

func adjustLon(x float64) float64 {
	if math.Abs(x) <= spi {
		return x
	}
	sign := 1.0
	if x < 0 {
		sign = -1
	}
	return x - sign*2*math.Pi
}

// ToMercator projects lon/lat degrees to Web Mercator metres.
func ToMercator(p orb.Point) orb.Point {
	lon, lat := p[0]*d2r, p[1]*d2r
	return orb.Point{radius * adjustLon(lon), radius * math.Log(math.Tan(math.Pi/4+0.5*lat))}
}

// ToLonLat projects Web Mercator metres to lon/lat degrees.
func ToLonLat(p orb.Point) orb.Point {
	lon := adjustLon(p[0] / radius)
	lat := math.Pi/2 - 2*math.Atan(math.Exp(-p[1]/radius))
	return orb.Point{lon * r2d, lat * r2d}
}

// ExtentToMercator projects lon/lat bounds the way the provider does for returnExtentOnly:
// latitude clamped to ±85.0511.
func ExtentToMercator(lon, lat float64) (float64, float64) {
	lat = math.Max(math.Min(lat, 85.0511), -85.0511)
	return (lon * math.Pi / 180) * radius, math.Log(math.Tan(math.Pi/4+(lat*math.Pi)/360)) * radius
}

// BoxToLonLat is the provider's inverse for the bbox prefilter.
func BoxToLonLat(x, y float64) (float64, float64) {
	return (x / radius) * (180 / math.Pi), (2*math.Atan(math.Exp(y/radius)) - math.Pi/2) * (180 / math.Pi)
}

// Projected is a copy of g with f applied to every point; g is not changed.
func Projected(g orb.Geometry, f func(orb.Point) orb.Point) orb.Geometry {
	switch g := g.(type) {
	case orb.Point:
		return f(g)
	case orb.MultiPoint:
		return orb.MultiPoint(mapped(g, f))
	case orb.LineString:
		return orb.LineString(mapped(g, f))
	case orb.Ring:
		return orb.Ring(mapped(g, f))
	case orb.MultiLineString:
		out := make(orb.MultiLineString, len(g))
		for i, l := range g {
			out[i] = mapped(l, f)
		}
		return out
	case orb.Polygon:
		return projectedPolygon(g, f)
	case orb.MultiPolygon:
		out := make(orb.MultiPolygon, len(g))
		for i, p := range g {
			out[i] = projectedPolygon(p, f)
		}
		return out
	}
	return project.Geometry(orb.Clone(g), f)
}

func projectedPolygon(p orb.Polygon, f func(orb.Point) orb.Point) orb.Polygon {
	out := make(orb.Polygon, len(p))
	for i, r := range p {
		out[i] = mapped(r, f)
	}
	return out
}

func mapped(ps []orb.Point, f func(orb.Point) orb.Point) []orb.Point {
	if ps == nil {
		return nil
	}
	out := make([]orb.Point, len(ps))
	for i, p := range ps {
		out[i] = f(p)
	}
	return out
}

// Bounds covers every coordinate of g, holes included (orb's Polygon.Bound uses the outer ring).
func Bounds(g orb.Geometry) orb.Bound {
	b := orb.Bound{Min: orb.Point{math.Inf(1), math.Inf(1)}, Max: orb.Point{math.Inf(-1), math.Inf(-1)}}
	extend(&b, g)
	return b
}

func extend(b *orb.Bound, g orb.Geometry) {
	switch g := g.(type) {
	case orb.Point:
		extendRun(b, []orb.Point{g})
	case orb.MultiPoint:
		extendRun(b, g)
	case orb.LineString:
		extendRun(b, g)
	case orb.Ring:
		extendRun(b, g)
	case orb.MultiLineString:
		for _, l := range g {
			extendRun(b, l)
		}
	case orb.Polygon:
		for _, r := range g {
			extendRun(b, r)
		}
	case orb.MultiPolygon:
		for _, p := range g {
			extend(b, p)
		}
	}
}

func extendRun(b *orb.Bound, ps []orb.Point) {
	for _, p := range ps {
		b.Min[0], b.Max[0] = min(b.Min[0], p[0]), max(b.Max[0], p[0])
		b.Min[1], b.Max[1] = min(b.Min[1], p[1]), max(b.Max[1], p[1])
	}
}
