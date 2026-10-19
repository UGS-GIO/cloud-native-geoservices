package query

import (
	"math"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/js"
)

// Spatial is a request's geometry filter: the query geometry, prepared, and its spatialRel. With
// a distance, intersects is being within the distance of the geometry, and the other relations
// test the geometry buffered by the distance, as AGOL does.
type Spatial struct {
	q    *geom.Prepared
	rel  string
	near *geom.Near
}

// maxBuffer is about the largest distance, in meters, AGOL buffers by.
const maxBuffer = 1.98e7

// newSpatial reads the filter; nil when the request has none.
func newSpatial(geometry, inSR any, rel string, distance any, units string) (*Spatial, error) {
	g, err := filterGeometry(geometry, inSR)
	if g == nil || err != nil {
		return nil, err
	}
	d := js.Number(distance)
	if math.IsNaN(d) || d < 0 || geom.Meters(d, units) > maxBuffer {
		return nil, Bad("'distance' parameter is invalid")
	}
	if d > 0 && !bufferable(g) {
		return nil, Bad("Unable to perform query. Please check your parameters.")
	}
	s := &Spatial{rel: rel}
	if m := geom.Meters(d, units); d > 0 && (rel == "" || rel == "esriSpatialRelIntersects") {
		s.near = geom.NewNear(g, m)
	} else if d > 0 {
		if g, err = geom.Buffer(g, m); err != nil {
			return nil, Bad("Unable to perform query. Please check your parameters.")
		}
	}
	s.q = geom.Prepare(g)
	return s, nil
}

// bufferable reports g has a point and no line of one point, which AGOL needs to buffer it.
func bufferable(g orb.Geometry) bool {
	lines, _ := g.(orb.MultiLineString)
	if l, ok := g.(orb.LineString); ok {
		lines = orb.MultiLineString{l}
	}
	for _, l := range lines {
		if len(l) < 2 {
			return false
		}
	}
	for range geom.Points(g) {
		return true
	}
	return false
}

// Spatial is the request's geometry filter, read as Run reads it.
func (r Raw) Spatial() (*Spatial, error) {
	g, ok := r["geometry"]
	if !ok || g == "" {
		return nil, nil
	}
	var inSR any
	if s := r["inSR"]; s != "" {
		inSR = coerce(s)
	}
	rel, _ := coerce(r["spatialRel"]).(string)
	var distance any
	if d := r["distance"]; d != "" {
		distance = coerce(d)
	}
	return newSpatial(coerce(g), inSR, rel, distance, r["units"])
}

// Decide answers for a feature from its bbox alone when that settles it.
func (s *Spatial) Decide(b orb.Bound) (pass, sure bool) {
	if s.near != nil {
		return false, !b.Intersects(s.near.Bound)
	}
	switch s.rel {
	case "esriSpatialRelEnvelopeIntersects":
		return b.Intersects(s.q.Bound), true
	case "esriSpatialRelWithin":
		return false, !b.Contains(s.q.Bound.Min) || !b.Contains(s.q.Bound.Max)
	case "esriSpatialRelContains":
		if !s.q.Bound.Contains(b.Min) || !s.q.Bound.Contains(b.Max) {
			return false, true
		}
	}
	switch s.q.Place(b) {
	case geom.Outside:
		return false, true
	case geom.Inside:
		return true, true
	}
	return false, false
}

// Covers reports that every feature whose bbox lies in b passes for sure, as Decide would find.
func (s *Spatial) Covers(b orb.Bound) bool {
	return s.rel != "esriSpatialRelWithin" && s.q.Place(b) == geom.Inside
}

// Match is the spatialRel test of a feature's geometry.
func (s *Spatial) Match(g orb.Geometry) bool {
	if g == nil {
		return false
	}
	if s.near != nil {
		return s.near.Match(g)
	}
	if pass, sure := s.Decide(geom.Bounds(g)); sure {
		return pass
	}
	switch s.rel {
	case "esriSpatialRelContains":
		return s.q.Contains(g)
	case "esriSpatialRelWithin":
		return s.q.Within(g)
	}
	return geom.Intersects(s.q.G, g)
}
