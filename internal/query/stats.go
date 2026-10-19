package query

import (
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/js"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
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
		return Bad("Invalid outStatistics")
	}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return Bad("Invalid outStatistics")
		}
		kind, _ := m["statisticType"].(string)
		field, _ := m["onStatisticField"].(string)
		name, _ := m["outStatisticFieldName"].(string)
		if name == "" {
			name = kind + "_" + field
		}
		a := aggregate{kind: strings.ToUpper(kind), name: space.ReplaceAllString(name, "_")}
		switch a.kind {
		case "COUNT", "SUM", "MIN", "MAX", "AVG", "STDDEV", "VAR":
		default:
			return Bad("Unsupported statisticType: " + kind)
		}
		if a.field, a.prop, ok = w.c.field(field); !ok {
			return Bad("Invalid onStatisticField: " + field)
		}
		w.aggregates = append(w.aggregates, a)
	}
	if s, ok := w.p.String("groupByFieldsForStatistics"); ok {
		for _, g := range strings.Split(s, ",") {
			g = strings.TrimSpace(g)
			if _, _, ok := w.c.field(g); !ok {
				return Bad("Invalid groupByFieldsForStatistics: " + g)
			}
			w.groupBy = append(w.groupBy, g)
		}
	}
	return nil
}

// accumulator computes the statistics of one field's non-null values, as SQL aggregates do:
// stddev and var are the sample ones, and every statistic but count is null without values.
type accumulator struct {
	count      int64
	nums       int64
	sum, sumSq float64
	min, max   any // numbers, or strings in where.CompareText order
}

func (a *accumulator) add(v any) {
	if v == nil {
		return
	}
	if a.count == 0 || compareKeys(v, a.min, false) < 0 {
		a.min = v
	}
	if a.count == 0 || compareKeys(v, a.max, false) > 0 {
		a.max = v
	}
	a.count++
	if x, ok := js.AsNumber(v); ok {
		a.sum += x
		a.sumSq += x * x
		a.nums++
	}
}

func (a *accumulator) result(kind string) any {
	n := float64(a.nums)
	switch {
	case kind == "COUNT":
		return a.count
	case kind == "MIN":
		return number(a.min)
	case kind == "MAX":
		return number(a.max)
	case a.nums == 0 || a.nums == 1 && (kind == "VAR" || kind == "STDDEV"):
		return nil
	case kind == "SUM":
		return a.sum
	case kind == "AVG":
		return a.sum / n
	}
	// Round-off can leave a constant column a hair below zero.
	v := max(0, (a.sumSq-a.sum*a.sum/n)/(n-1))
	if kind == "STDDEV" {
		return math.Sqrt(v)
	}
	return v
}

// number is an integer as a float64, as statistics are written.
func number(v any) any {
	if x, ok := v.(int64); ok {
		return float64(x)
	}
	return v
}

type group struct {
	values []any
	accs   []accumulator
}

// statistics groups the features and computes outStatistics, as winnow's aggregate SQL did.
func (w *winnow) statistics(idx []int) (*Response, error) {
	groupProps := make([]int, len(w.groupBy))
	for i, g := range w.groupBy {
		_, groupProps[i], _ = w.c.field(g)
	}
	var groups []*group
	byKey := map[string]*group{}
	var key []byte
	for _, i := range idx {
		ft := &w.c.Features[i]
		values := make([]any, len(groupProps))
		key = key[:0]
		for j, p := range groupProps {
			if p >= 0 {
				values[j] = ft.Props[p]
			}
			key = jsonx.Append(key, values[j]) // JSON keeps null apart from "null"
		}
		g, ok := byKey[string(key)]
		if !ok {
			g = &group{values: values, accs: make([]accumulator, len(w.aggregates))}
			byKey[string(key)] = g
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
				va, vb := getFold(a, k.name), getFold(b, k.name)
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
				f, _ := esri.GroupField(w.c.Meta, kv.K)
				fields = append(fields, f)
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

// getFold is a row's value for a key in any case.
func getFold(row jsonx.Obj, key string) any {
	for _, kv := range row {
		if strings.EqualFold(kv.K, key) {
			return kv.V
		}
	}
	return nil
}

// outputName reports a statistic's output name, which orderByFields may sort by.
func (w *winnow) outputName(name string) bool {
	return slices.ContainsFunc(w.aggregates, func(a aggregate) bool { return strings.EqualFold(a.name, name) })
}

func isDateName(dates map[string]bool, key string) bool {
	for name := range dates {
		if strings.HasSuffix(key, name) {
			return true
		}
	}
	return false
}
