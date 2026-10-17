package geom

import (
	"slices"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
)

// EsriKind is an Esri geometry type.
type EsriKind uint8

const (
	EsriPoint EsriKind = iota
	EsriMultipoint
	EsriPolyline
	EsriPolygon
)

// Esri is an Esri JSON geometry: a point, or parts (multipoint points as one part, paths, rings).
type Esri struct {
	Kind  EsriKind
	X, Y  float64
	Parts [][]orb.Point
}

// ToEsri converts a GeoJSON-shaped geometry as @terraformer/arcgis does: rings closed, outer
// rings clockwise and holes counterclockwise, rings under four points dropped, and each
// multipolygon member's rings written in reverse order.
func ToEsri(g orb.Geometry) (Esri, bool) {
	switch g := g.(type) {
	case orb.Point:
		return Esri{Kind: EsriPoint, X: g[0], Y: g[1]}, true
	case orb.MultiPoint:
		return Esri{Kind: EsriMultipoint, Parts: [][]orb.Point{g}}, true
	case orb.LineString:
		return Esri{Kind: EsriPolyline, Parts: [][]orb.Point{g}}, true
	case orb.MultiLineString:
		return Esri{Kind: EsriPolyline, Parts: parts(g)}, true
	case orb.Polygon:
		return Esri{Kind: EsriPolygon, Parts: orient(g)}, true
	case orb.MultiPolygon:
		var parts [][]orb.Point
		for _, p := range g {
			rings := orient(p)
			for i := len(rings) - 1; i >= 0; i-- {
				parts = append(parts, rings[i])
			}
		}
		return Esri{Kind: EsriPolygon, Parts: parts}, true
	}
	return Esri{}, false
}

// CloseRing repeats a ring's first point at its end when it is not there already.
func CloseRing(r []orb.Point) []orb.Point {
	if len(r) > 0 && r[0] != r[len(r)-1] {
		out := make([]orb.Point, len(r), len(r)+1)
		copy(out, r)
		return append(out, r[0])
	}
	return r
}

// Clockwise is a ring's winding by the shoelace sum, as @terraformer/arcgis tests it.
func Clockwise(r []orb.Point) bool {
	total := 0.0
	for i := 0; i < len(r)-1; i++ {
		total += (r[i+1][0] - r[i][0]) * (r[i+1][1] + r[i][1])
	}
	return total >= 0
}

func reversed(r []orb.Point) []orb.Point {
	out := slices.Clone(r)
	slices.Reverse(out)
	return out
}

func orient(p orb.Polygon) [][]orb.Point {
	if len(p) == 0 {
		return nil
	}
	outer := CloseRing(p[0])
	if len(outer) < 4 {
		return [][]orb.Point{}
	}
	if !Clockwise(outer) {
		outer = reversed(outer)
	}
	out := [][]orb.Point{outer}
	for _, h := range p[1:] {
		hole := CloseRing(h)
		if len(hole) >= 4 {
			if Clockwise(hole) {
				hole = reversed(hole)
			}
			out = append(out, hole)
		}
	}
	return out
}

// AppendJSON writes the geometry as Esri JSON (without a spatialReference, as winnow leaves it).
func (e Esri) AppendJSON(buf []byte) []byte {
	switch e.Kind {
	case EsriPoint:
		buf = append(buf, `{"x":`...)
		buf = jsonx.AppendNumber(buf, e.X)
		buf = append(buf, `,"y":`...)
		buf = jsonx.AppendNumber(buf, e.Y)
		return append(buf, '}')
	case EsriMultipoint:
		var pts []orb.Point
		if len(e.Parts) > 0 {
			pts = e.Parts[0]
		}
		return append(appendPoints(append(buf, `{"points":`...), pts), '}')
	case EsriPolyline:
		buf = append(buf, `{"paths":`...)
	default:
		buf = append(buf, `{"rings":`...)
	}
	return append(jsonx.AppendArray(buf, e.Parts, appendPoints[[]orb.Point]), '}')
}

func appendPoints[S ~[]orb.Point](buf []byte, ps S) []byte {
	buf = append(buf, '[')
	for i, p := range ps {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendPoint(buf, p)
	}
	return append(buf, ']')
}

func appendPoint(buf []byte, p orb.Point) []byte {
	buf = append(buf, '[')
	buf = jsonx.AppendNumber(buf, p[0])
	buf = append(buf, ',')
	buf = jsonx.AppendNumber(buf, p[1])
	return append(buf, ']')
}

func appendRings(buf []byte, p orb.Polygon) []byte {
	return jsonx.AppendArray(buf, p, appendPoints[orb.Ring])
}

// GeoJSON writes a geometry as a GeoJSON object.
type GeoJSON struct{ G orb.Geometry }

// AppendJSON implements jsonx.Appender.
func (g GeoJSON) AppendJSON(buf []byte) []byte {
	switch x := g.G.(type) {
	case orb.Point:
		buf = appendPoint(append(buf, `{"type":"Point","coordinates":`...), x)
	case orb.MultiPoint:
		buf = appendPoints(append(buf, `{"type":"MultiPoint","coordinates":`...), x)
	case orb.LineString:
		buf = appendPoints(append(buf, `{"type":"LineString","coordinates":`...), x)
	case orb.MultiLineString:
		buf = jsonx.AppendArray(append(buf, `{"type":"MultiLineString","coordinates":`...), x, appendPoints[orb.LineString])
	case orb.Polygon:
		buf = appendRings(append(buf, `{"type":"Polygon","coordinates":`...), x)
	case orb.MultiPolygon:
		buf = jsonx.AppendArray(append(buf, `{"type":"MultiPolygon","coordinates":`...), x, appendRings)
	default:
		return append(buf, "null"...)
	}
	return append(buf, '}')
}
