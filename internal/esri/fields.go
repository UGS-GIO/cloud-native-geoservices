package esri

import (
	"slices"
	"strings"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
)

// FieldType is the Esri field type for a provider type.
func FieldType(t string) string {
	switch strings.ToLower(t) {
	case "double":
		return "esriFieldTypeDouble"
	case "integer":
		return "esriFieldTypeInteger"
	case "date":
		return "esriFieldTypeDate"
	}
	return "esriFieldTypeString"
}

// Field is one field description, in Koop's key order.
type Field struct {
	Name, Type string
	Obj        jsonx.Obj
}

func definitionField(name, typ string) Field {
	t := FieldType(typ)
	o := jsonx.Obj{{K: "name", V: name}, {K: "type", V: t}, {K: "alias", V: name},
		{K: "sqlType", V: "sqlTypeOther"}, {K: "domain", V: nil}, {K: "defaultValue", V: nil}}
	switch t {
	case "esriFieldTypeString":
		o = append(o, jsonx.KV{K: "length", V: 128})
	case "esriFieldTypeDate":
		o = append(o, jsonx.KV{K: "length", V: 36})
	}
	return Field{name, t, o}
}

func idField(name string) Field {
	return Field{name, "esriFieldTypeOID", jsonx.Obj{{K: "name", V: name}, {K: "type", V: "esriFieldTypeOID"},
		{K: "alias", V: name}, {K: "sqlType", V: "sqlTypeInteger"}, {K: "domain", V: nil}, {K: "defaultValue", V: nil}}}
}

// QueryFields are the fields of a query response: the id field first, then the rest in schema
// order, kept to outFields unless it is empty or `*`.
func QueryFields(m *Metadata, outFields string) []Field {
	id := m.IDField
	idName := "OBJECTID"
	for _, c := range m.Fields {
		if c.Name == id {
			idName = id
		}
	}
	out := []Field{idField(idName)}
	for _, c := range m.Fields {
		if c.Name != id {
			out = append(out, definitionField(c.Name, c.Type))
		}
	}
	if outFields == "" || outFields == "*" {
		return out
	}
	want := map[string]bool{}
	for _, f := range strings.Split(outFields, ",") {
		want[strings.ToLower(strings.TrimSpace(f))] = true
	}
	return slices.DeleteFunc(out, func(f Field) bool { return !want[strings.ToLower(f.Name)] })
}

// LayerFields are the layer description's fields: every field, marked not editable or nullable.
func LayerFields(m *Metadata) []any {
	fields := QueryFields(m, "")
	out := make([]any, len(fields))
	for i, f := range fields {
		out[i] = append(f.Obj, jsonx.KV{K: "editable", V: false}, jsonx.KV{K: "nullable", V: false})
	}
	return out
}

// GroupField is a statistics group-by field, described from the layer's definition.
func GroupField(m *Metadata, name string) (Field, bool) {
	for _, c := range m.Fields {
		if strings.EqualFold(c.Name, name) {
			return definitionField(c.Name, c.Type), true
		}
	}
	return Field{}, false
}

// StatisticField is an outStatistics result field: a count is an integer, a min or max has the
// type of its source field, and the rest are doubles.
func StatisticField(name, kind string, m *Metadata, source string) Field {
	t, sql := "esriFieldTypeDouble", "sqlTypeFloat"
	length := 0
	switch {
	case kind == "COUNT":
		t, sql = "esriFieldTypeInteger", "sqlTypeInteger"
	case kind == "MIN" || kind == "MAX":
		for _, c := range m.Fields {
			if c.Name != source {
				continue
			}
			switch FieldType(c.Type) {
			case "esriFieldTypeInteger":
				t, sql = "esriFieldTypeInteger", "sqlTypeInteger"
			case "esriFieldTypeDouble":
				sql = "sqlTypeDouble"
			case "esriFieldTypeDate":
				t, sql, length = "esriFieldTypeDate", "sqlTypeDate", 8
			default:
				t, sql, length = "esriFieldTypeString", "sqlTypeNVarchar", 128
			}
		}
	}
	o := jsonx.Obj{{K: "name", V: name}, {K: "type", V: t}, {K: "alias", V: name}, {K: "sqlType", V: sql}}
	if length != 0 {
		o = append(o, jsonx.KV{K: "length", V: length})
	}
	return Field{name, t, append(o, jsonx.KV{K: "domain", V: nil}, jsonx.KV{K: "defaultValue", V: nil})}
}
