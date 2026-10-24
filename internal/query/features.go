package query

import (
	"github.com/UGS-GIO/cloud-native-geoservices/internal/esri"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/geom"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/jsonx"
)

// EsriFeature is one feature of a response.
type EsriFeature struct {
	Attrs   []jsonx.KV
	Geom    *geom.Esri
	HasGeom bool // write a geometry key (null when Geom is nil)
}

// AppendJSON implements jsonx.Appender.
func (f EsriFeature) AppendJSON(buf []byte) []byte { return f.appendJSON(buf, nil, nil) }

// appendJSON writes the feature with its geometry on t's grid when t is set, and the centroid AGOL
// adds when that geometry collapses.
func (f EsriFeature) appendJSON(buf []byte, t *geom.Transform, scratch *[]int64) []byte {
	buf = append(buf, `{"attributes":`...)
	buf = jsonx.Append(buf, jsonx.Obj(f.Attrs))
	if f.HasGeom {
		buf = append(buf, `,"geometry":`...)
		switch {
		case f.Geom == nil:
			buf = append(buf, "null"...)
		case t == nil:
			buf = f.Geom.AppendJSON(buf)
		default:
			var ok bool
			if buf, ok = f.Geom.AppendQuantized(buf, t, scratch); !ok {
				x, y := t.Centroid(f.Geom)
				buf = geom.AppendXY(append(buf, `,"centroid":`...), x, y)
			}
		}
	}
	return append(buf, '}')
}

// FeatureSet is a features response.
type FeatureSet struct {
	IDField      string
	GeometryType string // Esri name; empty when unknown
	SR           SpatialRef
	Fields       []esri.Field
	Features     []EsriFeature
	Exceeded     bool
	NoGeometry   bool            // a statistics result
	Transform    *geom.Transform // quantizationParameters' grid, nil when not quantized
}

// AppendJSON implements jsonx.Appender, in AGOL's key order.
func (s *FeatureSet) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"objectIdFieldName":`...)
	buf = jsonx.AppendString(buf, s.IDField)
	buf = append(buf, `,"uniqueIdField":{"name":`...)
	buf = jsonx.AppendString(buf, s.IDField)
	buf = append(buf, `,"isSystemMaintained":true},"globalIdFieldName":""`...)
	if s.GeometryType != "" {
		buf = append(buf, `,"geometryType":`...)
		buf = jsonx.AppendString(buf, s.GeometryType)
	}
	buf = append(buf, `,"spatialReference":`...)
	buf = s.SR.AppendJSON(buf)
	if s.Transform != nil {
		buf = s.Transform.AppendJSON(append(buf, `,"transform":`...))
	}
	buf = append(buf, `,"fields":`...)
	buf = jsonx.AppendArray(buf, s.Fields, func(b []byte, f esri.Field) []byte { return jsonx.Append(b, f.Obj) })
	if s.Exceeded {
		buf = append(buf, `,"exceededTransferLimit":true`...)
	}
	buf = append(buf, `,"features":`...)
	var scratch []int64
	buf = jsonx.AppendArray(buf, s.Features, func(b []byte, f EsriFeature) []byte { return f.appendJSON(b, s.Transform, &scratch) })
	return append(buf, '}')
}

// AppendJSON implements jsonx.Appender.
func (ids *IDs) AppendJSON(buf []byte) []byte {
	o := jsonx.Obj{{K: "objectIdFieldName", V: ids.IDField}, {K: "objectIds", V: ids.IDs}}
	if ids.Exceeded {
		o.Set("exceededTransferLimit", true)
	}
	return jsonx.Append(buf, o)
}
