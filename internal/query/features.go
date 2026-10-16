package query

import (
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
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
	Exceeded     Tri
	NoGeometry   bool // a statistics result
}

// AppendJSON implements jsonx.Appender, in Koop's key order.
func (s *FeatureSet) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"objectIdFieldName":`...)
	buf = jsonx.AppendString(buf, s.IDField)
	buf = append(buf, `,"uniqueIdField":{"name":`...)
	buf = jsonx.AppendString(buf, s.IDField)
	buf = append(buf, `,"isSystemMaintained":true},"globalIdFieldName":"","hasZ":false,"hasM":false,"spatialReference":`...)
	buf = s.SR.AppendJSON(buf)
	buf = append(buf, `,"fields":[`...)
	for i, f := range s.Fields {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = jsonx.Append(buf, f.Obj)
	}
	buf = append(buf, `],"features":[`...)
	for i, f := range s.Features {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = f.AppendJSON(buf)
	}
	buf = append(buf, `],"exceededTransferLimit":`...)
	buf = s.Exceeded.AppendJSON(buf)
	if s.GeometryType != "" {
		buf = append(buf, `,"geometryType":`...)
		buf = jsonx.AppendString(buf, s.GeometryType)
	}
	return append(buf, '}')
}

// AppendJSON implements jsonx.Appender.
func (ids *IDs) AppendJSON(buf []byte) []byte {
	return jsonx.Append(buf, jsonx.Obj{{K: "objectIdFieldName", V: ids.IDField}, {K: "objectIds", V: ids.IDs}})
}
