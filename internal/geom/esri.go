package geom

import (
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
		parts := make([][]orb.Point, len(g))
		for i, l := range g {
			parts[i] = l
		}
		return Esri{Kind: EsriPolyline, Parts: parts}, true
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

func closeRing(r []orb.Point) []orb.Point {
	if len(r) > 0 && r[0] != r[len(r)-1] {
		out := make([]orb.Point, len(r), len(r)+1)
		copy(out, r)
		return append(out, r[0])
	}
	return r
}

func clockwise(r []orb.Point) bool {
	total := 0.0
	for i := 0; i < len(r)-1; i++ {
		total += (r[i+1][0] - r[i][0]) * (r[i+1][1] + r[i][1])
	}
	return total >= 0
}

func reversed(r []orb.Point) []orb.Point {
	out := make([]orb.Point, len(r))
	for i, p := range r {
		out[len(r)-1-i] = p
	}
	return out
}

func orient(p orb.Polygon) [][]orb.Point {
	if len(p) == 0 {
		return nil
	}
	outer := closeRing(p[0])
	if len(outer) < 4 {
		return [][]orb.Point{}
	}
	if !clockwise(outer) {
		outer = reversed(outer)
	}
	out := [][]orb.Point{outer}
	for _, h := range p[1:] {
		hole := closeRing(h)
		if len(hole) >= 4 {
			if clockwise(hole) {
				hole = reversed(hole)
			}
			out = append(out, hole)
		}
	}
	return out
}

// AppendJSON writes the geometry as Esri JSON (without a spatialReference, as winnow leaves it).
func (e Esri) AppendJSON(buf []byte) []byte {
	if e.Kind == EsriPoint {
		buf = append(buf, `{"x":`...)
		buf = jsonx.AppendNumber(buf, e.X)
		buf = append(buf, `,"y":`...)
		buf = jsonx.AppendNumber(buf, e.Y)
		return append(buf, '}')
	}
	switch e.Kind {
	case EsriMultipoint:
		buf = append(buf, `{"points":`...)
		buf = appendPoints(buf, e.Parts[0])
		return append(buf, '}')
	case EsriPolyline:
		buf = append(buf, `{"paths":`...)
	default:
		buf = append(buf, `{"rings":`...)
	}
	buf = append(buf, '[')
	for i, part := range e.Parts {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendPoints(buf, part)
	}
	return append(buf, "]}"...)
}

func appendPoints(buf []byte, ps []orb.Point) []byte {
	buf = append(buf, '[')
	for i, p := range ps {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '[')
		buf = jsonx.AppendNumber(buf, p[0])
		buf = append(buf, ',')
		buf = jsonx.AppendNumber(buf, p[1])
		buf = append(buf, ']')
	}
	return append(buf, ']')
}

// GeoJSON writes a geometry as a GeoJSON object.
type GeoJSON struct{ G orb.Geometry }

// AppendJSON implements jsonx.Appender.
func (g GeoJSON) AppendJSON(buf []byte) []byte {
	switch x := g.G.(type) {
	case orb.Point:
		buf = append(buf, `{"type":"Point","coordinates":`...)
		buf = appendPoint(buf, x)
	case orb.MultiPoint:
		buf = append(buf, `{"type":"MultiPoint","coordinates":`...)
		buf = appendPoints(buf, x)
	case orb.LineString:
		buf = append(buf, `{"type":"LineString","coordinates":`...)
		buf = appendPoints(buf, x)
	case orb.MultiLineString:
		buf = append(buf, `{"type":"MultiLineString","coordinates":[`...)
		for i, l := range x {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendPoints(buf, l)
		}
		buf = append(buf, ']')
	case orb.Polygon:
		buf = append(buf, `{"type":"Polygon","coordinates":`...)
		buf = appendRings(buf, x)
	case orb.MultiPolygon:
		buf = append(buf, `{"type":"MultiPolygon","coordinates":[`...)
		for i, p := range x {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendRings(buf, p)
		}
		buf = append(buf, ']')
	default:
		return append(buf, "null"...)
	}
	return append(buf, '}')
}

func appendPoint(buf []byte, p orb.Point) []byte {
	buf = append(buf, '[')
	buf = jsonx.AppendNumber(buf, p[0])
	buf = append(buf, ',')
	buf = jsonx.AppendNumber(buf, p[1])
	return append(buf, ']')
}

func appendRings(buf []byte, p orb.Polygon) []byte {
	buf = append(buf, '[')
	for i, r := range p {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendPoints(buf, r)
	}
	return append(buf, ']')
}
