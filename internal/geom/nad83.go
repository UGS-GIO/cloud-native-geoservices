package geom

import (
	"math"

	"github.com/paulmach/orb"
)

// ArcGIS converts between WGS 84 and NAD83 (EPSG:4269) with WGS_1984_(ITRF00)_To_NAD_1983
// (108190), a coordinate frame Helmert transformation: shifts in metres, rotations in arc
// seconds, scale in parts per million.
var itrf00ToNAD83 = [7]float64{0.9956, -1.9013, -0.5215, 0.025915, 0.009426, 0.011599, 0.00062}

const (
	wgs84Flattening = 1 / 298.257223563
	grs80Flattening = 1 / 298.257222101
)

// ToNAD83 converts a WGS 84 lon/lat to NAD83.
func ToNAD83(p orb.Point) orb.Point { return helmert(p, 1, wgs84Flattening, grs80Flattening) }

// FromNAD83 converts a NAD83 lon/lat to WGS 84.
func FromNAD83(p orb.Point) orb.Point { return helmert(p, -1, grs80Flattening, wgs84Flattening) }

// helmert applies the transformation (or with sign -1 its inverse, close enough at these
// magnitudes) to a point on the ellipsoid surface.
func helmert(p orb.Point, sign, from, to float64) orb.Point {
	t := itrf00ToNAD83
	as := sign * math.Pi / 180 / 3600
	rx, ry, rz := t[3]*as, t[4]*as, t[5]*as
	m := 1 + sign*t[6]*1e-6
	x, y, z := toECEF(p, from)
	// Coordinate frame rotation: the transpose of the position vector convention.
	return fromECEF(sign*t[0]+m*(x+rz*y-ry*z), sign*t[1]+m*(-rz*x+y+rx*z), sign*t[2]+m*(ry*x-rx*y+z), to)
}

func toECEF(p orb.Point, f float64) (float64, float64, float64) {
	e2 := f * (2 - f)
	lon, lat := p[0]*d2r, p[1]*d2r
	n := radius / math.Sqrt(1-e2*math.Sin(lat)*math.Sin(lat))
	return n * math.Cos(lat) * math.Cos(lon), n * math.Cos(lat) * math.Sin(lon), n * (1 - e2) * math.Sin(lat)
}

func fromECEF(x, y, z, f float64) orb.Point {
	e2 := f * (2 - f)
	p := math.Hypot(x, y)
	lat := math.Atan2(z, p*(1-e2))
	for range 5 {
		n := radius / math.Sqrt(1-e2*math.Sin(lat)*math.Sin(lat))
		h := p/math.Cos(lat) - n
		lat = math.Atan2(z, p*(1-e2*n/(n+h)))
	}
	return orb.Point{math.Atan2(y, x) * r2d, lat * r2d}
}
