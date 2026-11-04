// Package pbf encodes query results as the Esri FeatureCollection protocol buffer (f=pbf):
// Esri's FeatureCollection.proto, esriPBuffer.
package pbf

import (
	"math"
	"slices"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/esri"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/geom"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/js"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/jsonx"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/query"
)

// transform is the response's quantization, or the default for its spatial reference.
func transform(fs *query.FeatureSet) *geom.Transform {
	if fs.Transform != nil {
		return fs.Transform
	}
	scale := 1.0
	switch fs.SR.Wkid {
	case 4326, 4269:
		scale = 1e-9
	case 102100, 3857, 102113, 3785:
		scale = 0.0001
	}
	return &geom.Transform{XScale: scale, YScale: scale}
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

// wrap nests body as the result field of a query result, in one allocation of the exact size:
// the response is often megabytes, and body may be a pooled buffer.
func wrap(result protowire.Number, body []byte) []byte {
	inner := protowire.SizeTag(result) + protowire.SizeBytes(len(body))
	out := make([]byte, 0, protowire.SizeTag(2)+protowire.SizeBytes(inner))
	out = protowire.AppendTag(out, 2, protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(inner))
	out = protowire.AppendTag(out, result, protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(len(body)))
	return append(out, body...)
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
	"esriFieldTypeGUID": 10, "esriFieldTypeGlobalID": 11, "esriFieldTypeXML": 12, "esriFieldTypeBigInteger": 13}

// Features encodes a features result.
func Features(fs *query.FeatureSet) []byte {
	t := transform(fs)
	fields := slices.Clone(fs.Fields)
	slices.SortStableFunc(fields, func(a, b esri.Field) int { return strings.Compare(a.Name, b.Name) })
	types := make(map[string]string, len(fields))
	for _, f := range fields {
		types[f.Name] = f.Type
	}

	sc := scratches.Get().(*scratch)
	defer sc.release()
	b := sc.body[:0]
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
	if fs.Exceeded {
		b = boolean(b, 9, true)
	}
	scale := double(double(nil, 1, t.XScale), 2, t.YScale)
	translate := double(double(nil, 1, t.XTranslate), 2, t.YTranslate)
	tb := message(message(nil, 2, scale), 3, translate)
	if t.LowerLeft {
		tb = varint(tb, 1, 1)
	}
	b = message(b, 12, tb)
	for _, f := range fields {
		fb := str(nil, 1, f.Name)
		if ft, ok := fieldTypes[f.Type]; ok {
			fb = varint(fb, 2, ft)
		}
		fb = str(fb, 3, f.Name)
		b = message(b, 13, fb)
	}
	fb, vb, gb := sc.fb[:0], sc.vb[:0], sc.gb[:0]
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
			var ok bool
			gb, ok = sc.geometry(gb[:0], ft.Geom, t)
			fb = message(fb, 2, gb)
			if !ok {
				x, y := t.Centroid(ft.Geom)
				centroid := packed(nil, 2, []uint32{1}, length)
				fb = message(fb, 4, packed(centroid, 3, []int64{x, y}, protowire.EncodeZigZag))
			}
		}
		b = message(b, 15, fb)
	}
	sc.body, sc.fb, sc.vb, sc.gb = b, fb, vb, gb
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

// value encodes one attribute in the Value field its field type picks.
func value(b []byte, fieldType string, v any) []byte {
	if v == nil {
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
	case "esriFieldTypeBigInteger":
		if n, ok := v.(int64); ok {
			return varint(b, 8, protowire.EncodeZigZag(n))
		}
		if f := js.Number(v); !math.IsNaN(f) {
			return varint(b, 8, protowire.EncodeZigZag(int64(f)))
		}
		return b
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

// scratch holds the buffers one encoding reuses from feature to feature, and that the next
// encoding reuses after it: a dense tile otherwise grows them from nothing every time.
type scratch struct {
	lengths          []uint32
	coords           []int64
	body, fb, vb, gb []byte
}

var scratches = sync.Pool{New: func() any { return new(scratch) }}

// maxPooled is the most a pooled buffer keeps; a rare huge tile's buffers go to the collector.
const maxPooled = 8 << 20

func (s *scratch) release() {
	if cap(s.coords)*8 > maxPooled || cap(s.body) > maxPooled || cap(s.fb) > maxPooled || cap(s.gb) > maxPooled {
		return
	}
	scratches.Put(s)
}

func (s *scratch) geometry(b []byte, g *geom.Esri, t *geom.Transform) ([]byte, bool) {
	if g.Memo != nil && (g.Kind == geom.EsriPolyline || g.Kind == geom.EsriPolygon) {
		if kx, ky, ok := aligned(t); ok {
			if st := s.stepsOf(g, t); st != nil {
				return st.append(b, kx, ky), true
			}
		}
	}
	lengths, coords, ok := t.Quantized(g, s.lengths[:0], s.coords[:0])
	if g.Kind == geom.EsriPoint {
		lengths[0] = 2
	}
	s.lengths, s.coords = lengths, coords
	b = packed(b, 2, lengths, length)
	return packed(b, 3, coords, protowire.EncodeZigZag), ok
}

func length(l uint32) uint64 { return uint64(l) }

// packed writes xs as one packed varint field, or nothing when there are none.
func packed[T any](b []byte, num protowire.Number, xs []T, enc func(T) uint64) []byte {
	if len(xs) == 0 {
		return b
	}
	size := 0
	for _, x := range xs {
		size += protowire.SizeVarint(enc(x))
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	b = protowire.AppendVarint(b, uint64(size))
	for _, x := range xs {
		b = protowire.AppendVarint(b, enc(x))
	}
	return b
}

// steps is a cached geometry quantized on a grid of one scale at the origin, encoded: its parts'
// lengths, each part's first point, and every later point's step from the one before, as packed
// varints. A tile's grid is that grid shifted by whole steps, so only the first points change
// from tile to tile, and the tile reuses the rest as bytes.
type steps struct {
	lengths []byte     // the whole lengths field
	first   [][2]int64 // per part
	rest    []byte     // every part's steps
	off     []int      // part j's steps are rest[off[j]:off[j+1]]
}

// aligned reports t as upper-left with its translate on whole steps of its scale from the
// origin, as Map Viewer's tiles are, and the whole steps of each axis.
func aligned(t *geom.Transform) (kx, ky int64, ok bool) {
	if t.LowerLeft || t.XScale <= 0 || t.YScale <= 0 {
		return 0, 0, false
	}
	fx, fy := t.XTranslate/t.XScale, t.YTranslate/t.YScale
	rx, ry := math.Round(fx), math.Round(fy)
	// Past this, a point rounding near a half step could land on another step than it would
	// from the tile's own translate.
	const slack = 1e-8
	if math.Abs(fx-rx) > slack || math.Abs(fy-ry) > slack || math.Abs(rx) > 1<<52 || math.Abs(ry) > 1<<52 {
		return 0, 0, false
	}
	return int64(rx), int64(ry), true
}

// stepsOf is g's steps at t's scale, worked out once per scale; nil when no part survives (the
// caller then draws a centroid).
func (s *scratch) stepsOf(g *geom.Esri, t *geom.Transform) *steps {
	key := [2]float64{t.XScale, t.YScale}
	if v, ok := g.Memo.Get(key); ok {
		return v.(*steps)
	}
	origin := geom.Transform{XScale: t.XScale, YScale: t.YScale}
	lengths, coords, ok := origin.Quantized(g, s.lengths[:0], s.coords[:0])
	s.lengths, s.coords = lengths, coords
	var st *steps
	if ok {
		st = &steps{lengths: packed(nil, 2, lengths, length), first: make([][2]int64, len(lengths)), off: make([]int, len(lengths)+1)}
		at := 0
		for j, n := range lengths {
			st.first[j] = [2]int64{coords[at], coords[at+1]}
			for _, c := range coords[at+2 : at+2*int(n)] {
				st.rest = protowire.AppendVarint(st.rest, protowire.EncodeZigZag(c))
			}
			st.off[j+1] = len(st.rest)
			at += 2 * int(n)
		}
	}
	n := int64(64)
	if st != nil {
		n += int64(len(st.lengths) + 16*len(st.first) + len(st.rest) + 8*len(st.off))
	}
	g.Memo.Put(key, st, n)
	return st
}

// append writes the geometry's lengths and coords fields on the grid shifted by kx, ky steps:
// x counts from the translate, y down from it.
func (st *steps) append(b []byte, kx, ky int64) []byte {
	b = append(b, st.lengths...)
	size := len(st.rest)
	for _, f := range st.first {
		size += protowire.SizeVarint(protowire.EncodeZigZag(f[0]-kx)) + protowire.SizeVarint(protowire.EncodeZigZag(f[1]+ky))
	}
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendVarint(b, uint64(size))
	for j, f := range st.first {
		b = protowire.AppendVarint(b, protowire.EncodeZigZag(f[0]-kx))
		b = protowire.AppendVarint(b, protowire.EncodeZigZag(f[1]+ky))
		b = append(b, st.rest[st.off[j]:st.off[j+1]]...)
	}
	return b
}
