// Package pbf encodes query results as the Esri FeatureCollection protocol buffer (f=pbf), field
// for field as Koop's encoder writes it (Esri's FeatureCollection.proto, esriPBuffer).
package pbf

import (
	"math"
	"slices"
	"strings"

	"github.com/paulmach/orb"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/query"
)

// Transform is the quantization applied to coordinates.
type Transform struct {
	XScale, YScale         float64
	XTranslate, YTranslate float64
}

// TransformFor is the client's quantization, or the default for the output spatial reference.
func TransformFor(q *query.Quantization, wkid int) Transform {
	if q != nil {
		return Transform{q.Scale, q.Scale, q.XTranslate, q.YTranslate}
	}
	scale := 1.0
	switch wkid {
	case 4326, 4269:
		scale = 1e-9
	case 102100, 3857, 102113, 3785:
		scale = 0.0001
	}
	return Transform{scale, scale, 0, 0}
}

func message(b []byte, num protowire.Number, body []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, body)
}

func str(b []byte, num protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func varint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func double(b []byte, num protowire.Number, f float64) []byte {
	b = protowire.AppendTag(b, num, protowire.Fixed64Type)
	return protowire.AppendFixed64(b, math.Float64bits(f))
}

func boolean(b []byte, num protowire.Number, v bool) []byte {
	return varint(b, num, protowire.EncodeBool(v))
}

func wrap(result protowire.Number, body []byte) []byte {
	return message(nil, 2, message(nil, result, body))
}

// Count encodes a count result.
func Count(n int64) []byte { return wrap(2, varint(nil, 1, uint64(n))) }

// IDs encodes an ids result.
func IDs(ids *query.IDs) []byte {
	b := str(nil, 1, ids.IDField)
	var packed []byte
	for _, v := range ids.IDs {
		if f, ok := js.AsNumber(v); ok {
			packed = protowire.AppendVarint(packed, uint64(int64(f)))
		}
	}
	if len(packed) > 0 {
		b = message(b, 3, packed)
	}
	return wrap(3, b)
}

var geometryTypes = map[string]uint64{"esriGeometryPoint": 0, "esriGeometryMultipoint": 1,
	"esriGeometryPolyline": 2, "esriGeometryPolygon": 3}

var fieldTypes = map[string]uint64{"esriFieldTypeSmallInteger": 0, "esriFieldTypeInteger": 1,
	"esriFieldTypeSingle": 2, "esriFieldTypeDouble": 3, "esriFieldTypeString": 4, "esriFieldTypeDate": 5,
	"esriFieldTypeOID": 6, "esriFieldTypeGeometry": 7, "esriFieldTypeBlob": 8, "esriFieldTypeRaster": 9,
	"esriFieldTypeGUID": 10, "esriFieldTypeGlobalID": 11, "esriFieldTypeXML": 12}

// Features encodes a features result with transform t.
func Features(fs *query.FeatureSet, t Transform) []byte {
	fields := slices.Clone(fs.Fields)
	slices.SortStableFunc(fields, func(a, b esri.Field) int { return strings.Compare(a.Name, b.Name) })
	types := make(map[string]string, len(fields))
	for _, f := range fields {
		types[f.Name] = f.Type
	}

	var b []byte
	if !fs.NoGeometry {
		b = str(b, 1, fs.IDField)
		u := str(nil, 1, fs.IDField)
		u = boolean(u, 2, true)
		b = message(b, 2, u)
	}
	gt, ok := geometryTypes[fs.GeometryType]
	if !ok {
		gt = 127
	}
	b = varint(b, 7, gt)
	if !fs.NoGeometry {
		b = message(b, 8, varint(nil, 1, uint64(uint32(fs.SR.Wkid))))
	}
	if fs.Exceeded != query.Null {
		b = boolean(b, 9, fs.Exceeded == query.True)
	}
	scale := double(double(nil, 1, t.XScale), 2, t.YScale)
	translate := double(double(nil, 1, t.XTranslate), 2, t.YTranslate)
	b = message(b, 12, message(message(nil, 2, scale), 3, translate))
	for _, f := range fields {
		fb := str(nil, 1, f.Name)
		if ft, ok := fieldTypes[f.Type]; ok {
			fb = varint(fb, 2, ft)
		}
		fb = str(fb, 3, f.Name)
		b = message(b, 13, fb)
	}
	var fb, vb, gb []byte
	// Attributes go out sorted by name. Features share one key order, so the sort is worked out
	// once and reused while the keys match.
	var keys []string
	var order []int
	var kinds []string
	for _, ft := range fs.Features {
		fb = fb[:0]
		if !sameKeys(keys, ft.Attrs) {
			keys, order, kinds = sortedOrder(ft.Attrs, types)
		}
		for k, i := range order {
			vb = value(vb[:0], kinds[k], ft.Attrs[i].V)
			fb = message(fb, 1, vb)
		}
		if ft.Geom != nil {
			gb = geometry(gb[:0], ft.Geom, t)
			fb = message(fb, 2, gb)
		}
		b = message(b, 15, fb)
	}
	return wrap(1, b)
}

func sameKeys(keys []string, attrs []jsonx.KV) bool {
	if keys == nil || len(keys) != len(attrs) {
		return false
	}
	for i, kv := range attrs {
		if kv.K != keys[i] {
			return false
		}
	}
	return true
}

// sortedOrder is the attribute keys, their indexes in name order, and each one's field type.
func sortedOrder(attrs []jsonx.KV, types map[string]string) (keys []string, order []int, kinds []string) {
	keys = make([]string, len(attrs))
	order = make([]int, len(attrs))
	for i, kv := range attrs {
		keys[i], order[i] = kv.K, i
	}
	slices.SortStableFunc(order, func(a, b int) int { return strings.Compare(keys[a], keys[b]) })
	kinds = make([]string, len(order))
	for k, i := range order {
		kinds[k] = types[keys[i]]
	}
	return keys, order, kinds
}

// toInt32 is JavaScript's `v | 0`.
func toInt32(f float64) int32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int32(int64(math.Mod(math.Trunc(f), 1<<32)))
}

// value encodes one attribute the way Koop picks its Value field from the field type.
func value(b []byte, fieldType string, v any) []byte {
	if v == nil {
		return b
	}
	if jsonx.IsUndefined(v) {
		return b
	}
	switch fieldType {
	case "esriFieldTypeOID":
		if f, ok := js.AsNumber(v); ok && f == math.Trunc(f) {
			return varint(b, 5, uint64(uint32(toInt32(f))))
		}
		return str(b, 1, js.String(v))
	case "esriFieldTypeInteger", "esriFieldTypeSmallInteger":
		return varint(b, 4, protowire.EncodeZigZag(int64(toInt32(js.Number(v)))))
	case "esriFieldTypeString", "esriFieldTypeGlobalID":
		return str(b, 1, js.String(v))
	case "esriFieldTypeDouble":
		return double(b, 3, js.Number(v))
	case "esriFieldTypeDate":
		f := js.Number(v)
		if math.IsNaN(f) {
			f = 0
		}
		return varint(b, 8, protowire.EncodeZigZag(int64(f)))
	}
	return b
}

// round is JavaScript's Math.round: halves go up.
func round(v float64) int64 {
	r := math.Floor(v)
	if v-r >= 0.5 {
		r++
	}
	return int64(r)
}

// quantize turns a run of points into deltas, dropping points that land on the previous one.
func quantize(pts []orb.Point, t Transform, coords []int64) ([]int64, int) {
	n := 0
	var px, py int64
	for i, p := range pts {
		x := round((p[0] - t.XTranslate) / t.XScale)
		y := round((t.YTranslate - p[1]) / t.YScale)
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

func geometry(b []byte, g *geom.Esri, t Transform) []byte {
	var lengths []uint32
	var coords []int64
	switch g.Kind {
	case geom.EsriPoint:
		coords = []int64{round((g.X - t.XTranslate) / t.XScale), round((t.YTranslate - g.Y) / t.YScale)}
		lengths = []uint32{2}
	case geom.EsriMultipoint:
		var n int
		coords, n = quantize(g.Parts[0], t, coords)
		lengths = []uint32{uint32(n)}
	default:
		for _, part := range g.Parts {
			before := len(coords)
			var n int
			coords, n = quantize(part, t, coords)
			if n > 1 {
				lengths = append(lengths, uint32(n))
			} else {
				coords = coords[:before]
			}
		}
	}
	b = packed(b, 2, lengths, func(l uint32) uint64 { return uint64(l) })
	return packed(b, 3, coords, protowire.EncodeZigZag)
}

// packed writes xs as one packed varint field, or nothing when there are none.
func packed[T any](b []byte, num protowire.Number, xs []T, enc func(T) uint64) []byte {
	if len(xs) == 0 {
		return b
	}
	var body []byte
	for _, x := range xs {
		body = protowire.AppendVarint(body, enc(x))
	}
	return message(b, num, body)
}
