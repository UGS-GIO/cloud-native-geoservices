package query

import (
	"github.com/paulmach/orb"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
)

// Spatial is a request's geometry filter: the query geometry, prepared, and its spatialRel.
type Spatial struct {
	q   *geom.Prepared
	rel string
}

// newSpatial reads the filter; nil when the request has none.
func newSpatial(geometry, inSR any, rel string) (*Spatial, error) {
	g, err := filterGeometry(geometry, inSR)
	if g == nil || err != nil {
		return nil, err
	}
	return &Spatial{geom.Prepare(g), rel}, nil
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
	return newSpatial(coerce(g), inSR, rel)
}

// Decide answers for a feature from its bbox alone when that settles it.
func (s *Spatial) Decide(b orb.Bound) (pass, sure bool) {
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
