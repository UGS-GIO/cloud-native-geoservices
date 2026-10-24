package geom

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/project"
)

// Wkid reads a spatial reference as AGOL does: a whole number, its digits, or an object (or its
// JSON) whose wkid is one of those; latestWkid is ignored.
func Wkid(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return int(x), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n, true
		}
		var m map[string]any
		if json.Unmarshal([]byte(x), &m) == nil {
			return Wkid(m)
		}
	case map[string]any:
		if w, ok := x["wkid"].(float64); ok {
			return Wkid(w)
		}
		if w, ok := x["wkid"].(string); ok {
			return Wkid(w)
		}
	}
	return 0, false
}

// Mercator wkids: what we project to and from.
var Mercator = map[int]bool{102100: true, 102113: true, 3857: true, 3785: true, 900913: true}

// ToMercator projects lon/lat degrees to Web Mercator metres.
var ToMercator orb.Projection = project.WGS84.ToMercator

// ToLonLat projects Web Mercator metres to lon/lat degrees.
var ToLonLat orb.Projection = project.Mercator.ToWGS84

// ExtentToMercator projects a corner of a returnExtentOnly envelope: latitude clamped to ±85.0511.
func ExtentToMercator(lon, lat float64) (float64, float64) {
	p := ToMercator(orb.Point{lon, math.Max(math.Min(lat, 85.0511), -85.0511)})
	return p[0], p[1]
}

// BoxToLonLat is the inverse for the bbox prefilter.
func BoxToLonLat(x, y float64) (float64, float64) {
	p := ToLonLat(orb.Point{x, y})
	return p[0], p[1]
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
