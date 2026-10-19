package query

import (
	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
)

// EsriFeature is one feature of a response.
type EsriFeature struct {
	Attrs   []jsonx.KV
	Geom    *geom.Esri
	HasGeom bool // write a geometry key (null when Geom is nil)
}

// AppendJSON implements jsonx.Appender.
func (f EsriFeature) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"attributes":`...)
	buf = jsonx.Append(buf, jsonx.Obj(f.Attrs))
	if f.HasGeom {
		buf = append(buf, `,"geometry":`...)
		if f.Geom != nil {
			buf = f.Geom.AppendJSON(buf)
		} else {
			buf = append(buf, "null"...)
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
	NoGeometry   bool // a statistics result
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
	buf = append(buf, `,"fields":`...)
	buf = jsonx.AppendArray(buf, s.Fields, func(b []byte, f esri.Field) []byte { return jsonx.Append(b, f.Obj) })
	if s.Exceeded {
		buf = append(buf, `,"exceededTransferLimit":true`...)
	}
	buf = append(buf, `,"features":`...)
	buf = jsonx.AppendArray(buf, s.Features, func(b []byte, f EsriFeature) []byte { return f.AppendJSON(b) })
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
