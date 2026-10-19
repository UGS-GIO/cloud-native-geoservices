package conformance

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Answer is one server's answer to a request, ready to compare.
type Answer struct {
	Tree    map[string]any
	Quantum float64 // coordinate step of an f=pbf answer
}

// Normalize parses an answer and removes the differences that carry no meaning: the object id
// field's name, false or null flags that clients read as a missing key, row order where the
// request sets none, ring start points and part order, and Web Mercator where the request names
// no outSR (the reference stores Web Mercator, the copy EPSG:4326).
func Normalize(req Request, status int, body []byte, pbf bool, idField string) (Answer, error) {
	var a Answer
	var err error
	if pbf {
		a.Tree, a.Quantum, err = DecodePBF(body)
	} else if err = json.Unmarshal(body, &a.Tree); err == nil {
		a.Quantum = dequantize(a.Tree)
	}
	if err != nil {
		return a, fmt.Errorf("%s: %w", req.Name, err)
	}
	if a.Tree == nil {
		a.Tree = map[string]any{}
	}
	if idField != "feature_id" {
		a.Tree = renameID(a.Tree, idField, false).(map[string]any)
	}
	a.Tree["_status"] = float64(status)
	for _, k := range []string{"exceededTransferLimit", "hasZ", "hasM"} {
		if v, ok := a.Tree[k]; ok && (v == nil || v == false) {
			delete(a.Tree, k)
		}
	}
	if _, ok := req.Params["outSR"]; !ok {
		if isMercator(a.Tree["spatialReference"]) {
			a.Quantum /= metersPerDegree
		}
		toWGS84(a.Tree)
	}
	geometries(a.Tree)
	_, ordered := req.Params["orderByFields"]
	if features, ok := a.Tree["features"].([]any); ok {
		if req.Rows == "count" {
			a.Tree["features"] = float64(len(features))
		} else if !ordered {
			sortRows(features)
		}
	}
	props, _ := a.Tree["properties"].(map[string]any)
	for _, m := range []map[string]any{a.Tree, props} {
		if ids, ok := m["objectIds"].([]any); ok {
			slices.SortFunc(ids, compareAny)
		}
	}
	return a, nil
}

// dequantize turns an f=json answer's quantized geometries and centroids back into coordinates,
// as DecodePBF does, and returns the largest step of its transform.
func dequantize(tree map[string]any) float64 {
	t, _ := tree["transform"].(map[string]any)
	scale, _ := t["scale"].([]any)
	translate, _ := t["translate"].([]any)
	if len(scale) < 2 || len(translate) < 2 {
		return 0
	}
	sx, _ := scale[0].(float64)
	sy, _ := scale[1].(float64)
	tx, _ := translate[0].(float64)
	ty, _ := translate[1].(float64)
	if t["originPosition"] != "lowerLeft" {
		sy = -sy
	}
	point := func(x, y float64) (float64, float64) { return tx + x*sx, ty + y*sy }
	features, _ := tree["features"].([]any)
	for _, f := range features {
		fm, _ := f.(map[string]any)
		g, _ := fm["geometry"].(map[string]any)
		eachPoint(fm["centroid"], point)
		if _, ok := g["x"]; ok {
			eachPoint(g, point)
		}
		parts, _ := g["paths"].([]any)
		if r, ok := g["rings"].([]any); ok {
			parts = r
		}
		if pts, ok := g["points"].([]any); ok {
			parts = []any{pts}
		}
		for _, part := range parts {
			var x, y float64
			pts, _ := part.([]any)
			for i, p := range pts {
				xy, _ := p.([]any)
				if len(xy) < 2 {
					continue
				}
				dx, _ := xy[0].(float64)
				dy, _ := xy[1].(float64)
				x, y = x+dx, y+dy
				px, py := point(x, y)
				pts[i] = []any{px, py}
			}
		}
	}
	return math.Max(math.Abs(sx), math.Abs(sy))
}

// renameID names the object id field feature_id in keys and names, but not in attribute values.
func renameID(v any, id string, inAttributes bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			// A FeatureCollection's own properties (ids, count) are not attributes.
			attrs := inAttributes || k == "attributes" || k == "properties" && x["type"] != "FeatureCollection"
			out[strings.ReplaceAll(k, id, "feature_id")] = renameID(child, id, attrs)
		}
		return out
	case []any:
		for i := range x {
			x[i] = renameID(x[i], id, inAttributes)
		}
	case string:
		if !inAttributes {
			return strings.ReplaceAll(x, id, "feature_id")
		}
	}
	return v
}

func isMercator(sr any) bool {
	m, _ := sr.(map[string]any)
	switch m["wkid"] {
	case 102100.0, 3857.0, 102113.0, 900913.0:
		return true
	}
	return false
}

const radius = 6378137.0

// metersPerDegree is the fewest Web Mercator meters in a degree, so it bounds a step in degrees.
const metersPerDegree = radius * math.Pi / 180

func unproject(x, y float64) (float64, float64) {
	return x / radius * 180 / math.Pi, (2*math.Atan(math.Exp(y/radius)) - math.Pi/2) * 180 / math.Pi
}

// toWGS84 converts every envelope, and the features of every result, that is in Web Mercator.
func toWGS84(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				toWGS84(x)
			}
		}
		return
	}
	if isMercator(m["spatialReference"]) {
		if _, ok := m["xmin"]; ok {
			m["xmin"], m["ymin"] = unprojectAny(m["xmin"], m["ymin"])
			m["xmax"], m["ymax"] = unprojectAny(m["xmax"], m["ymax"])
		}
		if features, ok := m["features"].([]any); ok {
			for _, f := range features {
				if fm, ok := f.(map[string]any); ok {
					eachPoint(fm["geometry"], unproject)
				}
			}
		}
		m["spatialReference"] = map[string]any{"wkid": 4326.0, "latestWkid": 4326.0}
	}
	for k, child := range m {
		if k != "features" {
			toWGS84(child)
		}
	}
}

func unprojectAny(x, y any) (any, any) {
	fx, ok1 := x.(float64)
	fy, ok2 := y.(float64)
	if !ok1 || !ok2 {
		return x, y
	}
	lon, lat := unproject(fx, fy)
	return lon, lat
}

// eachPoint rewrites every coordinate of an Esri JSON geometry.
func eachPoint(g any, f func(x, y float64) (float64, float64)) {
	m, ok := g.(map[string]any)
	if !ok {
		return
	}
	if x, ok := m["x"].(float64); ok {
		if y, ok := m["y"].(float64); ok {
			m["x"], m["y"] = f(x, y)
		}
	}
	var walk func(v any)
	walk = func(v any) {
		arr, ok := v.([]any)
		if !ok {
			return
		}
		if len(arr) >= 2 {
			x, okx := arr[0].(float64)
			y, oky := arr[1].(float64)
			if okx && oky {
				arr[0], arr[1] = f(x, y)
				return
			}
		}
		for _, c := range arr {
			walk(c)
		}
	}
	for _, k := range []string{"points", "paths", "rings"} {
		walk(m[k])
	}
}

// geometries puts each geometry's rings and parts in a canonical order.
func geometries(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			switch k {
			case "rings", "paths":
				if parts, ok := child.([]any); ok {
					canonicalParts(parts, k == "rings")
				}
			case "coordinates":
				if t, _ := x["type"].(string); t == "Polygon" || t == "MultiPolygon" {
					canonicalGeoJSON(child, t == "MultiPolygon")
				}
			default:
				geometries(child)
			}
		}
	case []any:
		for _, c := range x {
			geometries(c)
		}
	}
}

func canonicalParts(parts []any, rings bool) {
	if rings {
		for _, p := range parts {
			if ring, ok := p.([]any); ok {
				rotateRing(ring)
			}
		}
	}
	slices.SortStableFunc(parts, compareAny)
}

func canonicalGeoJSON(coords any, multi bool) {
	polys, ok := coords.([]any)
	if !ok {
		return
	}
	if !multi {
		polys = []any{coords}
	}
	for _, p := range polys {
		rings, _ := p.([]any)
		for _, r := range rings {
			if ring, ok := r.([]any); ok {
				rotateRing(ring)
			}
		}
	}
}

// rotateRing starts a closed ring at its smallest vertex, keeping its direction.
func rotateRing(ring []any) {
	n := len(ring) - 1
	if n < 2 || compareAny(ring[0], ring[n]) != 0 {
		return
	}
	best := 0
	for i := 1; i < n; i++ {
		if compareAny(ring[i], ring[best]) < 0 {
			best = i
		}
	}
	open := slices.Clone(ring[:n])
	for i := range n {
		ring[i] = open[(best+i)%n]
	}
	ring[n] = ring[0]
}

// rowKey is a row's object id, or nil.
func rowKey(row any) any {
	m, _ := row.(map[string]any)
	if id, ok := m["id"]; ok {
		return id
	}
	for _, k := range []string{"attributes", "properties"} {
		if attrs, ok := m[k].(map[string]any); ok {
			if id, ok := attrs["feature_id"]; ok {
				return id
			}
		}
	}
	return nil
}

// sortRows orders rows by object id, or by their content when they have none.
func sortRows(rows []any) {
	slices.SortStableFunc(rows, func(x, y any) int {
		kx, ky := rowKey(x), rowKey(y)
		if kx != nil && ky != nil {
			return compareAny(kx, ky)
		}
		return compareAny(x, y)
	})
}

// compareAny is a total order on JSON values: by kind, then by value.
func compareAny(x, y any) int {
	rank := func(v any) int {
		switch v.(type) {
		case nil:
			return 0
		case bool:
			return 1
		case float64:
			return 2
		case string:
			return 3
		case []any:
			return 4
		}
		return 5
	}
	if rx, ry := rank(x), rank(y); rx != ry {
		return rx - ry
	}
	switch a := x.(type) {
	case bool:
		b := y.(bool)
		if a == b {
			return 0
		} else if !a {
			return -1
		}
		return 1
	case float64:
		b := y.(float64)
		if a < b {
			return -1
		} else if a > b {
			return 1
		}
		return 0
	case string:
		return strings.Compare(a, y.(string))
	case []any:
		b := y.([]any)
		for i := range min(len(a), len(b)) {
			if c := compareAny(a[i], b[i]); c != 0 {
				return c
			}
		}
		return len(a) - len(b)
	case map[string]any:
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(y)
		return strings.Compare(string(ja), string(jb))
	}
	return 0
}
