package esri

import (
	"slices"
	"strings"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
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
		want[strings.TrimSpace(f)] = true
	}
	return slices.DeleteFunc(out, func(f Field) bool { return !want[f.Name] })
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
		if c.Name == name {
			return definitionField(c.Name, c.Type), true
		}
	}
	return Field{}, false
}

// ValueField describes a field from a sample value, as Koop does for unknown group-by fields.
func ValueField(name string, v any) Field {
	switch x := v.(type) {
	case int64:
		return definitionField(name, "Integer")
	case float64:
		if x == float64(int64(x)) {
			return definitionField(name, "Integer")
		}
		return definitionField(name, "Double")
	}
	return definitionField(name, "String")
}

// StatisticField is an outStatistics result field.
func StatisticField(name string, date bool) Field {
	t, sql := "esriFieldTypeDouble", "sqlTypeFloat"
	if date {
		t, sql = "esriFieldTypeDate", "sqlTypeOther"
	}
	return Field{name, t, jsonx.Obj{{K: "name", V: name}, {K: "type", V: t}, {K: "sqlType", V: sql},
		{K: "alias", V: name}, {K: "domain", V: nil}, {K: "defaultValue", V: nil}}}
}
