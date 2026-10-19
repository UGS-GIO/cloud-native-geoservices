package conformance

import (
	"errors"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// The field numbers and enums of Esri's FeatureCollection.proto (package esriPBuffer,
// https://github.com/Esri/arcgis-pbf/blob/main/proto/FeatureCollection/FeatureCollection.proto).
var (
	pbfGeometryTypes = []string{"esriGeometryPoint", "esriGeometryMultipoint", "esriGeometryPolyline",
		"esriGeometryPolygon", "esriGeometryMultipatch", "esriGeometryEnvelope"}
	pbfFieldTypes = []string{"esriFieldTypeSmallInteger", "esriFieldTypeInteger", "esriFieldTypeSingle",
		"esriFieldTypeDouble", "esriFieldTypeString", "esriFieldTypeDate", "esriFieldTypeOID",
		"esriFieldTypeGeometry", "esriFieldTypeBlob", "esriFieldTypeRaster", "esriFieldTypeGUID",
		"esriFieldTypeGlobalID", "esriFieldTypeXML", "esriFieldTypeBigInteger", "esriFieldTypeDateOnly",
		"esriFieldTypeTimeOnly", "esriFieldTypeTimestampOffset"}
	pbfSQLTypes = []string{"sqlTypeBigInt", "sqlTypeBinary", "sqlTypeBit", "sqlTypeChar", "sqlTypeDate",
		"sqlTypeDecimal", "sqlTypeDouble", "sqlTypeFloat", "sqlTypeGeometry", "sqlTypeGUID", "sqlTypeInteger",
		"sqlTypeLongNVarchar", "sqlTypeLongVarbinary", "sqlTypeLongVarchar", "sqlTypeNChar", "sqlTypeNVarchar",
		"sqlTypeOther", "sqlTypeReal", "sqlTypeSmallInt", "sqlTypeSqlXml", "sqlTypeTime", "sqlTypeTimestamp",
		"sqlTypeTimestamp2", "sqlTypeTinyInt", "sqlTypeVarbinary", "sqlTypeVarchar", "sqlTypeTimestampWithTimezone"}
)

var errPBF = errors.New("malformed pbf")

// field is one field of a protobuf message: its varint or fixed value, or its bytes.
type field struct {
	num   protowire.Number
	typ   protowire.Type
	v     uint64
	bytes []byte
}

func fields(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, errPBF
		}
		b = b[n:]
		f := field{num: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			f.v, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			f.v, n = protowire.ConsumeFixed64(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.v = uint64(v)
		case protowire.BytesType:
			f.bytes, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return nil, errPBF
		}
		out = append(out, f)
		b = b[n:]
	}
	return out, nil
}

func name(names []string, i uint64) any {
	if i < uint64(len(names)) {
		return names[i]
	}
	if i == 127 {
		return "esriGeometryNone"
	}
	return float64(i)
}

// packed reads a packed repeated varint field, zigzag decoding it when signed.
func packed(b []byte, signed bool) ([]int64, error) {
	var out []int64
	for len(b) > 0 {
		v, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return nil, errPBF
		}
		if signed {
			out = append(out, protowire.DecodeZigZag(v))
		} else {
			out = append(out, int64(v))
		}
		b = b[n:]
	}
	return out, nil
}

// DecodePBF turns an f=pbf response into the tree its f=json form has, with coordinates
// dequantized. quantum is the largest coordinate step of the transform.
func DecodePBF(b []byte) (tree map[string]any, quantum float64, err error) {
	top, err := fields(b)
	if err != nil {
		return nil, 0, err
	}
	tree = map[string]any{}
	for _, f := range top {
		if f.num != 2 {
			continue
		}
		results, err := fields(f.bytes)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range results {
			switch r.num {
			case 1:
				quantum, err = featureResult(r.bytes, tree)
			case 2:
				err = countResult(r.bytes, tree)
			case 3:
				err = idsResult(r.bytes, tree)
			case 4:
				err = extentCountResult(r.bytes, tree)
			}
			if err != nil {
				return nil, 0, err
			}
		}
	}
	return tree, quantum, nil
}

func countResult(b []byte, tree map[string]any) error {
	fs, err := fields(b)
	tree["count"] = 0.0
	for _, f := range fs {
		if f.num == 1 {
			tree["count"] = float64(f.v)
		}
	}
	return err
}

func idsResult(b []byte, tree map[string]any) error {
	fs, err := fields(b)
	if err != nil {
		return err
	}
	ids := []any{}
	for _, f := range fs {
		switch f.num {
		case 1:
			tree["objectIdFieldName"] = string(f.bytes)
		case 3:
			vs, err := packed(f.bytes, false)
			if err != nil {
				return err
			}
			for _, v := range vs {
				ids = append(ids, float64(v))
			}
		}
	}
	tree["objectIds"] = ids
	return nil
}

func extentCountResult(b []byte, tree map[string]any) error {
	fs, err := fields(b)
	if err != nil {
		return err
	}
	for _, f := range fs {
		switch f.num {
		case 1:
			if tree["extent"], err = envelope(f.bytes); err != nil {
				return err
			}
		case 2:
			tree["count"] = float64(f.v)
		}
	}
	return nil
}

func spatialReference(b []byte) (map[string]any, error) {
	fs, err := fields(b)
	out := map[string]any{}
	for _, f := range fs {
		switch f.num {
		case 1:
			out["wkid"] = float64(f.v)
		case 2:
			out["latestWkid"] = float64(f.v)
		case 5:
			out["wkt"] = string(f.bytes)
		}
	}
	return out, err
}

func envelope(b []byte) (map[string]any, error) {
	fs, err := fields(b)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	keys := map[protowire.Number]string{1: "xmin", 2: "ymin", 3: "xmax", 4: "ymax"}
	for _, f := range fs {
		if k, ok := keys[f.num]; ok {
			out[k] = math.Float64frombits(f.v)
		} else if f.num == 5 {
			if out["spatialReference"], err = spatialReference(f.bytes); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

type transform struct {
	upperLeft          bool
	xScale, yScale     float64
	xTranslate, yTrans float64
}

func readTransform(b []byte) (transform, error) {
	t := transform{upperLeft: true, xScale: 1, yScale: 1}
	fs, err := fields(b)
	if err != nil {
		return t, err
	}
	for _, f := range fs {
		switch f.num {
		case 1:
			t.upperLeft = f.v == 0
		case 2, 3:
			sub, err := fields(f.bytes)
			if err != nil {
				return t, err
			}
			for _, s := range sub {
				v := math.Float64frombits(s.v)
				switch {
				case f.num == 2 && s.num == 1:
					t.xScale = v
				case f.num == 2 && s.num == 2:
					t.yScale = v
				case f.num == 3 && s.num == 1:
					t.xTranslate = v
				case f.num == 3 && s.num == 2:
					t.yTrans = v
				}
			}
		}
	}
	return t, nil
}

func pbfField(b []byte) (map[string]any, error) {
	fs, err := fields(b)
	out := map[string]any{"type": pbfFieldTypes[0], "sqlType": pbfSQLTypes[0]}
	for _, f := range fs {
		switch f.num {
		case 1:
			out["name"] = string(f.bytes)
		case 2:
			out["type"] = name(pbfFieldTypes, f.v)
		case 3:
			out["alias"] = string(f.bytes)
		case 4:
			out["sqlType"] = name(pbfSQLTypes, f.v)
		case 5:
			out["domain"] = string(f.bytes)
		case 6:
			out["defaultValue"] = string(f.bytes)
		}
	}
	return out, err
}

func value(b []byte) (any, error) {
	fs, err := fields(b)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		switch f.num {
		case 1:
			return string(f.bytes), nil
		case 2:
			return float64(math.Float32frombits(uint32(f.v))), nil
		case 3:
			return math.Float64frombits(f.v), nil
		case 4:
			return float64(int32(protowire.DecodeZigZag(f.v & math.MaxUint32))), nil
		case 5, 7:
			return float64(f.v), nil
		case 6:
			return float64(int64(f.v)), nil
		case 8:
			return float64(protowire.DecodeZigZag(f.v)), nil
		case 9:
			return f.v != 0, nil
		case 10:
			return nil, nil
		}
	}
	return nil, nil
}

// geometry dequantizes a geometry into its Esri JSON form.
func geometry(b []byte, gt any, t transform) (map[string]any, error) {
	fs, err := fields(b)
	if err != nil {
		return nil, err
	}
	var lengths, coords []int64
	for _, f := range fs {
		switch f.num {
		case 1:
			gt = name(pbfGeometryTypes, f.v)
		case 2:
			lengths, err = packed(f.bytes, false)
		case 3:
			coords, err = packed(f.bytes, true)
		}
		if err != nil {
			return nil, err
		}
	}
	// Each part's deltas start again from the origin.
	starts := map[int]bool{0: true}
	at := 0
	for _, n := range lengths {
		at += int(n)
		starts[at] = true
	}
	var pts [][]any
	var x, y int64
	for i := 0; i+1 < len(coords); i += 2 {
		if starts[i/2] {
			x, y = 0, 0
		}
		x, y = x+coords[i], y+coords[i+1]
		px := t.xTranslate + float64(x)*t.xScale
		py := t.yTrans + float64(y)*t.yScale
		if t.upperLeft {
			py = t.yTrans - float64(y)*t.yScale
		}
		pts = append(pts, []any{px, py})
	}
	if gt == "esriGeometryPoint" && len(pts) == 1 {
		return map[string]any{"x": pts[0][0], "y": pts[0][1]}, nil
	}
	key := map[any]string{"esriGeometryMultipoint": "points", "esriGeometryPolyline": "paths"}[gt]
	if key == "" {
		key = "rings"
	}
	if key == "points" {
		out := []any{}
		for _, p := range pts {
			out = append(out, p)
		}
		return map[string]any{key: out}, nil
	}
	parts := []any{}
	for _, n := range lengths {
		if int(n) > len(pts) {
			return nil, errPBF
		}
		part := []any{}
		for _, p := range pts[:n] {
			part = append(part, p)
		}
		parts = append(parts, part)
		pts = pts[n:]
	}
	return map[string]any{key: parts}, nil
}

func featureResult(b []byte, tree map[string]any) (float64, error) {
	fs, err := fields(b)
	if err != nil {
		return 0, err
	}
	t := transform{xScale: 1, yScale: 1}
	var gt any = pbfGeometryTypes[0]
	var fieldList []any
	var rawFeatures [][]byte
	for _, f := range fs {
		switch f.num {
		case 1:
			tree["objectIdFieldName"] = string(f.bytes)
		case 2:
			u, err := fields(f.bytes)
			if err != nil {
				return 0, err
			}
			uid := map[string]any{"isSystemMaintained": false}
			for _, x := range u {
				if x.num == 1 {
					uid["name"] = string(x.bytes)
				} else if x.num == 2 {
					uid["isSystemMaintained"] = x.v != 0
				}
			}
			tree["uniqueIdField"] = uid
		case 3:
			tree["globalIdFieldName"] = string(f.bytes)
		case 7:
			gt = name(pbfGeometryTypes, f.v)
		case 8:
			if tree["spatialReference"], err = spatialReference(f.bytes); err != nil {
				return 0, err
			}
		case 9:
			tree["exceededTransferLimit"] = f.v != 0
		case 10:
			tree["hasZ"] = f.v != 0
		case 11:
			tree["hasM"] = f.v != 0
		case 12:
			if t, err = readTransform(f.bytes); err != nil {
				return 0, err
			}
		case 13:
			fd, err := pbfField(f.bytes)
			if err != nil {
				return 0, err
			}
			fieldList = append(fieldList, fd)
		case 15:
			rawFeatures = append(rawFeatures, f.bytes)
		}
	}
	tree["geometryType"] = gt
	tree["fields"] = fieldList
	features := []any{}
	for _, raw := range rawFeatures {
		fs, err := fields(raw)
		if err != nil {
			return 0, err
		}
		attrs := map[string]any{}
		feature := map[string]any{"attributes": attrs}
		i := 0
		for _, f := range fs {
			switch f.num {
			case 1:
				v, err := value(f.bytes)
				if err != nil {
					return 0, err
				}
				if i < len(fieldList) {
					attrs[fieldList[i].(map[string]any)["name"].(string)] = v
				}
				i++
			case 2:
				if feature["geometry"], err = geometry(f.bytes, gt, t); err != nil {
					return 0, err
				}
			case 4:
				if feature["centroid"], err = geometry(f.bytes, "esriGeometryPoint", t); err != nil {
					return 0, err
				}
			}
		}
		features = append(features, feature)
	}
	tree["features"] = features
	return math.Max(t.xScale, t.yScale), nil
}
