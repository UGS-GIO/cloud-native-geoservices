package geom

import (
	"math"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/project"
	sf "github.com/peterstace/simplefeatures/geom"
)

// unitMeters are the Esri distance units.
var unitMeters = map[string]float64{"esriSRUnit_Meter": 1, "esriSRUnit_Kilometer": 1000,
	"esriSRUnit_Foot": 0.3048, "esriSRUnit_StatuteMile": 1609.344, "esriSRUnit_NauticalMile": 1852,
	"esriSRUnit_USNauticalMile": 1853.248}

// Meters is a distance in units (meters when units is unknown or empty).
func Meters(distance float64, units string) float64 {
	if m, ok := unitMeters[units]; ok {
		return distance * m
	}
	return distance
}

// metersPerDegree are the WGS84 meridian and parallel lengths of a degree at lat.
func metersPerDegree(lat float64) (perLat, perLon float64) {
	const a, e2 = 6378137.0, 6.69437999014e-3
	s := math.Sin(lat * d2r)
	w := math.Sqrt(1 - e2*s*s)
	return a * (1 - e2) / (w * w * w) * d2r, a / w * math.Cos(lat*d2r) * d2r
}

// Pad grows a lon/lat box by meters on every side, by the shortest degree of longitude it spans.
func Pad(b [4]float64, meters float64) [4]float64 {
	perLat, _ := metersPerDegree(0)
	dLat := meters / perLat
	lat := math.Min(math.Max(math.Abs(b[1]), math.Abs(b[3]))+dLat, 89)
	_, perLon := metersPerDegree(lat)
	dLon := meters / perLon
	return [4]float64{b[0] - dLon, b[1] - dLat, b[2] + dLon, b[3] + dLat}
}

// Near tests whether features lie within a distance of a lon/lat geometry. Distances are planar in
// a projection about the geometry's center, scaled to the WGS84 ellipsoid there: within 0.1% for
// distances of tens of kilometers.
type Near struct {
	Bound  orb.Bound // the geometry's bounds grown by the distance
	meters float64
	center orb.Point
	kx, ky float64
	g      sf.Geometry
}

// NewNear readies the test of being within meters of g.
func NewNear(g orb.Geometry, meters float64) *Near {
	b := Bounds(g)
	n := &Near{meters: meters, center: b.Center()}
	n.ky, n.kx = metersPerDegree(n.center[1])
	p := Pad([4]float64{b.Min[0], b.Min[1], b.Max[0], b.Max[1]}, meters)
	n.Bound = orb.Bound{Min: orb.Point{p[0], p[1]}, Max: orb.Point{p[2], p[3]}}
	n.g = toSF(n.local(g))
	return n
}

func (n *Near) local(g orb.Geometry) orb.Geometry {
	return project.Geometry(orb.Clone(g), func(p orb.Point) orb.Point {
		return orb.Point{(p[0] - n.center[0]) * n.kx, (p[1] - n.center[1]) * n.ky}
	})
}

// Match reports g within the distance.
func (n *Near) Match(g orb.Geometry) bool {
	if g == nil || !Bounds(g).Intersects(n.Bound) {
		return false
	}
	d, ok := sf.Distance(n.g, toSF(n.local(g)))
	return ok && d <= n.meters
}
