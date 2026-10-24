// Package esri builds the ArcGIS REST JSON documents: service, layer and field descriptions,
// from defaults that follow ArcGIS Online and the provider's metadata.
package esri

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/jsonx"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/pq"
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
	// TileMaxRecordCount is the page of a resultType=tile query.
	TileMaxRecordCount int
	Renderer           json.RawMessage
	LabelingInfo       json.RawMessage
	MinScale           json.RawMessage
	MaxScale           json.RawMessage
	// Levels are the web map zoom levels the layer has generalized copies (overviews) for.
	Levels []int
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

// extent is the metadata extent, in lon/lat.
func extent(e *[4]float64) jsonx.Obj {
	return jsonx.Obj{{K: "xmin", V: e[0]}, {K: "ymin", V: e[1]}, {K: "xmax", V: e[2]}, {K: "ymax", V: e[3]},
		{K: "spatialReference", V: jsonx.Obj{{K: "wkid", V: 4326}, {K: "latestWkid", V: 4326}}}}
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

// set reports a render's override present and not null.
func set(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

// Layer is the layer description (`FeatureServer/{layer}`). layerID is the path segment.
func Layer(m *Metadata, layerID string) jsonx.Obj {
	o := defaults(layerDefaults)
	o.Set("fields", LayerFields(m))
	if id, err := strconv.Atoi(layerID); err == nil && id != 0 {
		o.Set("id", id)
	}
	o.Set("displayField", displayField(m))
	o.Set("uniqueIdField", jsonx.Obj{{K: "name", V: m.IDField}, {K: "isSystemMaintained", V: true}})
	gt := GeometryType(m.GeometryType)
	o.Set("geometryType", gt)
	if m.Extent != nil {
		o.Set("extent", extent(m.Extent))
	}
	drawing := jsonx.Obj{{K: "renderer", V: renderer(gt)}, {K: "labelingInfo", V: nil}}
	if set(m.Renderer) {
		drawing[0].V = jsonx.Raw(m.Renderer)
	}
	if set(m.LabelingInfo) {
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
		o.Set("standardMaxRecordCount", m.MaxRecordCount)
		o.Set("standardMaxRecordCountNoGeometry", m.MaxRecordCount)
	}
	if m.TileMaxRecordCount != 0 {
		o.Set("tileMaxRecordCount", m.TileMaxRecordCount)
	}
	if set(m.MinScale) {
		o.Set("minScale", jsonx.Raw(m.MinScale))
	}
	if set(m.MaxScale) {
		o.Set("maxScale", jsonx.Raw(m.MaxScale))
	}
	if len(m.Levels) > 0 {
		o.Set("multiScaleGeometryInfo", jsonx.Obj{{K: "levels", V: m.Levels}})
	}
	return o
}

// displayField is the first text field whose name has "name" in it, else the first text field,
// as ArcGIS Pro picks one when it publishes a layer.
func displayField(m *Metadata) string {
	first := ""
	for _, c := range m.Fields {
		if FieldType(c.Type) != "esriFieldTypeString" || c.Name == m.IDField {
			continue
		}
		if strings.Contains(strings.ToLower(c.Name), "name") {
			return c.Name
		}
		if first == "" {
			first = c.Name
		}
	}
	if first == "" {
		return m.IDField
	}
	return first
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
	var minScale, maxScale any = 0, 0
	if set(m.MinScale) {
		minScale = jsonx.Raw(m.MinScale)
	}
	if set(m.MaxScale) {
		maxScale = jsonx.Raw(m.MaxScale)
	}
	o.Set("layers", []any{jsonx.Obj{
		{K: "id", V: 0}, {K: "name", V: m.Name}, {K: "parentLayerId", V: -1}, {K: "defaultVisibility", V: true},
		{K: "subLayerIds", V: nil}, {K: "minScale", V: minScale}, {K: "maxScale", V: maxScale},
		{K: "type", V: "Feature Layer"}, {K: "geometryType", V: GeometryType(m.GeometryType)},
	}})
	return o
}
