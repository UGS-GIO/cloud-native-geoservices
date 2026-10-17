// Package esri builds the ArcGIS REST JSON documents Koop serves: service, layer and field
// descriptions, from Koop's defaults and the provider's metadata.
package esri

import (
	_ "embed"
	"encoding/json"
	"strconv"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
)

//go:embed defaults/server.json
var serverDefaults []byte

//go:embed defaults/layer.json
var layerDefaults []byte

// Metadata is what the provider says about a layer.
type Metadata struct {
	Name           string
	Description    string
	IDField        string
	GeometryType   string // Point, LineString or Polygon
	Fields         []pq.Column
	Extent         *[4]float64 // lon/lat
	MaxRecordCount int
	Renderer       json.RawMessage
	LabelingInfo   json.RawMessage
	MinScale       json.RawMessage
}

// GeometryType is the Esri name for the provider's geometry type.
func GeometryType(t string) string {
	switch t {
	case "Point":
		return "esriGeometryPoint"
	case "MultiPoint":
		return "esriGeometryMultipoint"
	case "LineString", "MultiLineString":
		return "esriGeometryPolyline"
	case "Polygon", "MultiPolygon":
		return "esriGeometryPolygon"
	}
	return ""
}

func defaults(raw []byte) jsonx.Obj {
	v, err := jsonx.Parse(raw)
	if err != nil {
		panic(err)
	}
	return v.(jsonx.Obj)
}

// extent is the metadata extent as Koop normalizes it (spatialReference first).
func extent(e *[4]float64) jsonx.Obj {
	return jsonx.Obj{{K: "spatialReference", V: jsonx.Obj{{K: "wkid", V: 4326}}},
		{K: "xmin", V: e[0]}, {K: "ymin", V: e[1]}, {K: "xmax", V: e[2]}, {K: "ymax", V: e[3]}}
}

func renderer(geometryType string) jsonx.Raw {
	switch geometryType {
	case "esriGeometryPolygon":
		return jsonx.Raw(`{"type":"simple","symbol":{"color":[75,172,198,161],"outline":{"color":[150,150,150,155],"width":0.5,"type":"esriSLS","style":"esriSLSSolid"},"type":"esriSFS","style":"esriSFSSolid"}}`)
	case "esriGeometryPolyline":
		return jsonx.Raw(`{"type":"simple","symbol":{"color":[247,150,70,204],"width":6.999999999999999,"type":"esriSLS","style":"esriSLSSolid"}}`)
	}
	return jsonx.Raw(`{"type":"simple","symbol":{"color":[247,150,70,161],"outline":{"color":[190,190,190,105],"width":0.5,"type":"esriSLS","style":"esriSLSSolid"},"size":7.5,"type":"esriSMS","style":"esriSMSCircle"}}`)
}

// truthy is how Koop decides whether a JSON override applies (lodash pickBy on the value).
func truthy(raw json.RawMessage) bool {
	switch string(raw) {
	case "", "null", "false", "0", `""`:
		return false
	}
	return true
}

// Layer is the layer description (`FeatureServer/{layer}`). layerID is the path segment.
func Layer(m *Metadata, layerID string) jsonx.Obj {
	o := defaults(layerDefaults)
	o.Set("fields", LayerFields(m))
	if id, err := strconv.Atoi(layerID); err == nil && id != 0 {
		o.Set("id", id)
	}
	o.Set("displayField", m.IDField)
	o.Set("uniqueIdField", jsonx.Obj{{K: "name", V: m.IDField}, {K: "isSystemMaintained", V: true}})
	gt := GeometryType(m.GeometryType)
	o.Set("geometryType", gt)
	if m.Extent != nil {
		o.Set("extent", extent(m.Extent))
	}
	drawing := jsonx.Obj{{K: "renderer", V: renderer(gt)}, {K: "labelingInfo", V: nil}}
	if truthy(m.Renderer) {
		drawing[0].V = jsonx.Raw(m.Renderer)
	}
	if truthy(m.LabelingInfo) {
		drawing[1].V = jsonx.Raw(m.LabelingInfo)
	}
	o.Set("drawingInfo", drawing)
	o.Set("name", m.Name)
	if m.Description != "" {
		o.Set("description", m.Description)
	}
	o.Set("objectIdField", m.IDField)
	if m.MaxRecordCount != 0 {
		o.Set("maxRecordCount", m.MaxRecordCount)
	}
	if truthy(m.MinScale) {
		o.Set("minScale", jsonx.Raw(m.MinScale))
	}
	return o
}

// Layers is the `FeatureServer/layers` document.
func Layers(m *Metadata) jsonx.Obj {
	return jsonx.Obj{{K: "layers", V: []any{Layer(m, "0")}}, {K: "tables", V: []any{}}}
}

// Server is the service description (`FeatureServer`).
func Server(m *Metadata) jsonx.Obj {
	o := defaults(serverDefaults)
	o.Set("serviceDescription", m.Description)
	o.Set("description", m.Description)
	if m.MaxRecordCount != 0 {
		o.Set("maxRecordCount", m.MaxRecordCount)
	}
	if m.Extent != nil {
		o.Set("fullExtent", extent(m.Extent))
		o.Set("initialExtent", extent(m.Extent))
	}
	var minScale any = 0
	if truthy(m.MinScale) {
		minScale = jsonx.Raw(m.MinScale)
	}
	o.Set("layers", []any{jsonx.Obj{
		{K: "id", V: 0}, {K: "name", V: m.Name}, {K: "type", V: "Feature Layer"},
		{K: "parentLayerId", V: -1}, {K: "defaultVisibility", V: true}, {K: "subLayerIds", V: nil},
		{K: "minScale", V: minScale}, {K: "maxScale", V: 0}, {K: "geometryType", V: GeometryType(m.GeometryType)},
	}})
	return o
}
