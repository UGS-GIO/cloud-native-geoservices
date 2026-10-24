package geom

import (
	"math"
	"strconv"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/jsonx"
)

// Transform is a quantization grid: whole steps of the scale from the translate. Upper-left
// origins count y down from the translate and round; lower-left origins count up and truncate,
// as ArcGIS Online does.
type Transform struct {
	XScale, YScale         float64
	XTranslate, YTranslate float64
	LowerLeft              bool
	Edit                   bool // edit mode: AGOL writes z and m terms in the JSON transform
}

// Round is JavaScript's Math.round: halves go up.
func Round(v float64) int64 {
	r := math.Floor(v)
	if v-r >= 0.5 {
		r++
	}
	return int64(r)
}

// Point is p on the grid.
func (t *Transform) Point(x, y float64) (int64, int64) {
	qx := Round((x - t.XTranslate) / t.XScale)
	if t.LowerLeft {
		return qx, int64(math.Trunc((y - t.YTranslate) / t.YScale))
	}
	return qx, Round((t.YTranslate - y) / t.YScale)
}

// Deltas appends a run of points as the first point and then steps, dropping points that land on
// the previous one, and reports how many it kept.
func (t *Transform) Deltas(pts []orb.Point, coords []int64) ([]int64, int) {
	n := 0
	var px, py int64
	for i, p := range pts {
		x, y := t.Point(p[0], p[1])
		if i == 0 {
			coords = append(coords, x, y)
		} else if x != px || y != py {
			coords = append(coords, x-px, y-py)
		} else {
			continue
		}
		px, py = x, y
		n++
	}
	return coords, n
}

// Quantized appends e on t's grid: each kept part's deltas to coords and its point count to
// lengths. A path or ring that collapses to one point is dropped; ok is false when none is left.
func (t *Transform) Quantized(e *Esri, lengths []uint32, coords []int64) ([]uint32, []int64, bool) {
	switch e.Kind {
	case EsriPoint:
		x, y := t.Point(e.X, e.Y)
		return append(lengths, 1), append(coords, x, y), true
	case EsriMultipoint:
		var n int
		if len(e.Parts) > 0 {
			coords, n = t.Deltas(e.Parts[0], coords)
		}
		return append(lengths, uint32(n)), coords, true
	}
	for _, part := range e.Parts {
		before := len(coords)
		var n int
		if coords, n = t.Deltas(part, coords); n > 1 {
			lengths = append(lengths, uint32(n))
		} else {
			coords = coords[:before]
		}
	}
	return lengths, coords, len(lengths) > 0
}

// Centroid is the center of e's envelope on t's grid.
func (t *Transform) Centroid(e *Esri) (x, y int64) {
	var b orb.Bound
	for i, p := range e.Parts {
		if i == 0 {
			b = orb.MultiPoint(p).Bound()
		} else {
			b = b.Union(orb.MultiPoint(p).Bound())
		}
	}
	c := b.Center()
	return t.Point(c[0], c[1])
}

// AppendJSON writes the transform as AGOL's f=json answer does.
func (t *Transform) AppendJSON(buf []byte) []byte {
	origin := "upperLeft"
	if t.LowerLeft {
		origin = "lowerLeft"
	}
	scale, translate := []float64{t.XScale, t.YScale}, []float64{t.XTranslate, t.YTranslate}
	if t.Edit {
		scale, translate = append(scale, 0, 0), append(translate, 0, 0)
	}
	return jsonx.Append(buf, jsonx.Obj{{K: "originPosition", V: origin}, {K: "scale", V: scale}, {K: "translate", V: translate}})
}

var quantizedKeys = map[EsriKind]string{EsriMultipoint: `{"points":`, EsriPolyline: `{"paths":[`, EsriPolygon: `{"rings":[`}

// AppendQuantized writes e on t's grid as Esri JSON, reusing scratch, and reports false when the
// geometry collapses.
func (e Esri) AppendQuantized(buf []byte, t *Transform, scratch *[]int64) ([]byte, bool) {
	lengths, coords, ok := t.Quantized(&e, nil, (*scratch)[:0])
	*scratch = coords
	if e.Kind == EsriPoint {
		return AppendXY(buf, coords[0], coords[1]), true
	}
	buf = append(buf, quantizedKeys[e.Kind]...)
	for i, n := range lengths {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '[')
		for j := range int(n) {
			if j > 0 {
				buf = append(buf, ',')
			}
			buf = append(strconv.AppendInt(append(buf, '['), coords[0], 10), ',')
			buf = append(strconv.AppendInt(buf, coords[1], 10), ']')
			coords = coords[2:]
		}
		buf = append(buf, ']')
	}
	if e.Kind != EsriMultipoint {
		buf = append(buf, ']')
	}
	return append(buf, '}'), ok
}

// AppendXY writes an integer point as Esri JSON.
func AppendXY(buf []byte, x, y int64) []byte {
	buf = strconv.AppendInt(append(buf, `{"x":`...), x, 10)
	buf = strconv.AppendInt(append(buf, `,"y":`...), y, 10)
	return append(buf, '}')
}
