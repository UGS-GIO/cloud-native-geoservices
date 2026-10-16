package query

import (
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
)

type aggregate struct {
	kind  string // COUNT, SUM, MIN, MAX, AVG, STDDEV, VAR
	field string
	name  string
	prop  int
}

var space = regexp.MustCompile(`\s`)

func (w *winnow) parseStatistics() error {
	v, ok := w.p["outStatistics"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return bad("Invalid outStatistics")
	}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return bad("Invalid outStatistics")
		}
		kind, _ := m["statisticType"].(string)
		field, _ := m["onStatisticField"].(string)
		name, _ := m["outStatisticFieldName"].(string)
		if name == "" {
			name = kind + "_" + field
		}
		a := aggregate{strings.ToUpper(kind), field, space.ReplaceAllString(name, "_"), w.c.propIndex(field)}
		switch a.kind {
		case "COUNT", "SUM", "MIN", "MAX", "AVG", "STDDEV", "VAR":
		default:
			return bad("Unsupported statisticType: " + kind)
		}
		w.aggregates = append(w.aggregates, a)
	}
	if s, ok := w.p.String("groupByFieldsForStatistics"); ok {
		for _, g := range strings.Split(s, ",") {
			w.groupBy = append(w.groupBy, strings.TrimSpace(g))
		}
	}
	return nil
}

type accumulator struct {
	count      int64
	sum, sumSq float64
	hasSum     bool
	min, max   float64
	hasMinMax  bool
	n          int64 // values seen, nulls included (stddev counts them as 0, like alasql)
	varCount   int64
	varSum     float64
	varSumSq   float64
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func (a *accumulator) add(v any) {
	x, isNum := number(v)
	a.n++
	if v != nil {
		a.count++
	}
	if isNum {
		a.sum += x
		a.hasSum = true
		if !a.hasMinMax || x < a.min {
			a.min = x
		}
		if !a.hasMinMax || x > a.max {
			a.max = x
		}
		a.hasMinMax = true
		a.sumSq += x * x
		a.varCount++
		a.varSum += x
		a.varSumSq += x * x
	}
}

func (a *accumulator) result(kind string) any {
	switch kind {
	case "COUNT":
		return a.count
	case "SUM":
		if !a.hasSum {
			return jsonx.Undefined
		}
		return a.sum
	case "MIN":
		if !a.hasMinMax {
			return jsonx.Undefined
		}
		return a.min
	case "MAX":
		if !a.hasMinMax {
			return jsonx.Undefined
		}
		return a.max
	case "AVG":
		if a.count == 0 {
			return jsonx.Undefined
		}
		return a.sum / float64(a.count)
	case "VAR":
		if a.varCount > 1 {
			// Round-off can leave a constant column a hair below zero.
			return max(0, (a.varSumSq-a.varSum*a.varSum/float64(a.varCount))/float64(a.varCount-1))
		}
		return 0.0
	case "STDDEV":
		if a.n == 0 {
			return 0.0
		}
		mean := a.sum / float64(a.n)
		return math.Sqrt(max(0, a.sumSq/float64(a.n)-mean*mean))
	}
	return jsonx.Undefined
}

type group struct {
	values []any
	accs   []accumulator
}

// statistics groups the features and computes outStatistics, as winnow's aggregate SQL did.
func (w *winnow) statistics(idx []int) (*Response, error) {
	groupProps := make([]int, len(w.groupBy))
	for i, g := range w.groupBy {
		groupProps[i] = w.c.propIndex(g)
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, i := range idx {
		ft := &w.c.Features[i]
		values := make([]any, len(groupProps))
		var key strings.Builder
		for j, p := range groupProps {
			if p >= 0 {
				values[j] = ft.Props[p]
			}
			key.WriteString(js.String(values[j])) // alasql joins the String() of each value
			key.WriteByte('`')
		}
		g, ok := byKey[key.String()]
		if !ok {
			g = &group{values: values, accs: make([]accumulator, len(w.aggregates))}
			byKey[key.String()] = g
			groups = append(groups, g)
		}
		for j, a := range w.aggregates {
			var v any
			if a.prop >= 0 {
				v = ft.Props[a.prop]
			}
			g.accs[j].add(v)
		}
	}
	if len(groups) == 0 && len(w.groupBy) == 0 {
		groups = []*group{{accs: make([]accumulator, len(w.aggregates))}}
	}
	rows := make([]jsonx.Obj, len(groups))
	for i, g := range groups {
		row := jsonx.Obj{}
		for j, a := range w.aggregates {
			row = append(row, jsonx.KV{K: a.name, V: g.accs[j].result(a.kind)})
		}
		for j, name := range w.groupBy {
			row = append(row, jsonx.KV{K: name, V: g.values[j]})
		}
		rows[i] = row
	}
	if len(w.order) > 0 {
		slices.SortStableFunc(rows, func(a, b jsonx.Obj) int {
			for _, k := range w.order {
				va, _ := a.Get(k.name)
				vb, _ := b.Get(k.name)
				if c := compareKeys(va, vb, k.desc); c != 0 {
					return c
				}
			}
			return 0
		})
	}
	if w.limit != 0 {
		rows = rows[min(len(rows), w.offset):min(len(rows), w.offset+w.limit)]
	}

	dateFields := map[string]bool{}
	for _, a := range w.aggregates {
		for _, f := range w.c.Meta.Fields {
			if f.Name == a.field && f.Type == "Date" {
				dateFields[a.name] = true
			}
		}
	}
	var fields []esri.Field
	if len(rows) > 0 {
		for _, kv := range rows[0] {
			if slices.Contains(w.groupBy, kv.K) {
				if f, ok := esri.GroupField(w.c.Meta, kv.K); ok {
					fields = append(fields, f)
				} else {
					fields = append(fields, esri.ValueField(kv.K, kv.V))
				}
				continue
			}
			fields = append(fields, esri.StatisticField(kv.K, isDateName(dateFields, kv.K)))
		}
	}
	feats := make([]EsriFeature, len(rows))
	jsonFeats := make([]any, len(rows))
	for i, r := range rows {
		feats[i] = EsriFeature{Attrs: r}
		jsonFeats[i] = jsonx.Obj{{K: "attributes", V: r}}
	}
	fieldObjs := make([]any, len(fields))
	for i, f := range fields {
		fieldObjs[i] = f.Obj
	}
	out := jsonx.Obj{{K: "displayFieldName", V: ""}, {K: "fields", V: fieldObjs}, {K: "features", V: jsonFeats}}
	return &Response{JSON: out, Features: &FeatureSet{Fields: fields, Features: feats, NoGeometry: true}}, nil
}

func isDateName(dates map[string]bool, key string) bool {
	for name := range dates {
		if strings.HasSuffix(key, name) {
			return true
		}
	}
	return false
}
