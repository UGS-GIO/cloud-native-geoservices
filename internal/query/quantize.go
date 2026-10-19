package query

import (
	"github.com/paulmach/orb"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
)

// Transform is the grid q puts an answer in sr on, given the layer's lon/lat extent; nil when q
// is in view mode with no extent to go on, which AGOL answers unquantized.
func (q Quantization) Transform(sr SpatialRef, layer *[4]float64) *geom.Transform {
	if q.Edit {
		// The default XY domain of the spatial reference, as AGOL stores it.
		t := &geom.Transform{XScale: 1e-9, YScale: 1e-9, XTranslate: -400, YTranslate: -400, LowerLeft: q.LowerLeft, Edit: true}
		if geom.Mercator[sr.Wkid] {
			t.XScale, t.YScale, t.XTranslate, t.YTranslate = 1e-4, 1e-4, -20037700, -30241100
		}
		return t
	}
	var b [4]float64
	switch {
	case q.Extent != nil:
		b = sr.from(q.ExtentSR, *q.Extent)
	case layer != nil:
		b, _ = box(sr.Envelope(*layer))
	default:
		return nil
	}
	t := &geom.Transform{XScale: q.Tolerance, YScale: q.Tolerance, XTranslate: b[0], YTranslate: b[3], LowerLeft: q.LowerLeft}
	if q.LowerLeft {
		t.YTranslate = b[1]
	}
	if q.Tolerance == 0 {
		t.XScale, t.YScale = (b[2]-b[0])/10000, (b[3]-b[1])/10000
	}
	if t.XScale == 0 || t.YScale == 0 {
		return nil
	}
	return t
}

// from projects an envelope's corners from wkid (0: the layer's lon/lat) into s.
func (s SpatialRef) from(wkid int, b [4]float64) [4]float64 {
	if wkid == s.Wkid || wkid == 0 && s.Wkid == 4326 || geom.Mercator[wkid] && geom.Mercator[s.Wkid] {
		return b
	}
	var toLonLat orb.Projection
	switch {
	case geom.Mercator[wkid]:
		toLonLat = geom.ToLonLat
	case wkid == 4269:
		toLonLat = geom.FromNAD83
	}
	to := s.project()
	for i := 0; i < 4; i += 2 {
		p := orb.Point{b[i], b[i+1]}
		if toLonLat != nil {
			p = toLonLat(p)
		}
		if to != nil {
			p = to(p)
		}
		b[i], b[i+1] = p[0], p[1]
	}
	return b
}
