package conformance

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

// Diff is one way an answer differs from the reference, with every row's ids and array indexes
// folded into [] so that one cause is one diff.
type Diff struct {
	Path    string // for example features[].attributes.Measure
	Kind    string // missing (only the reference has it), extra (only we have it), value, length or order
	Example string
	Count   int
}

func (d Diff) String() string { return fmt.Sprintf("%s (%s)", d.Path, d.Kind) }

type differ struct {
	diffs   map[string]*Diff
	quantum float64
	ordered bool
}

// Compare lists how ours differs from the reference. ordered says the request set a row order.
func Compare(ref, ours Answer, ordered bool) []Diff {
	d := &differ{diffs: map[string]*Diff{}, quantum: math.Max(ref.Quantum, ours.Quantum), ordered: ordered}
	d.walk("", ref.Tree, ours.Tree, false)
	out := make([]Diff, 0, len(d.diffs))
	for _, v := range d.diffs {
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b Diff) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func show(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 80 {
		return string(b[:77]) + "..."
	}
	return string(b)
}

func (d *differ) add(path, kind string, ref, ours any) {
	key := path + " " + kind
	if x, ok := d.diffs[key]; ok {
		x.Count++
		return
	}
	ex := ""
	switch kind {
	case "missing":
		ex = "esri=" + show(ref)
	case "extra":
		ex = "ours=" + show(ours)
	default:
		ex = "esri=" + show(ref) + " ours=" + show(ours)
	}
	d.diffs[key] = &Diff{Path: path, Kind: kind, Example: ex, Count: 1}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (d *differ) walk(path string, ref, ours any, geom bool) {
	switch r := ref.(type) {
	case map[string]any:
		o, ok := ours.(map[string]any)
		if !ok {
			d.add(path, "value", ref, ours)
			return
		}
		for k, rv := range r {
			ov, ok := o[k]
			if !ok {
				d.add(join(path, k), "missing", rv, nil)
				continue
			}
			d.walk(join(path, k), rv, ov, geom || k == "geometry" || k == "centroid")
		}
		for k, ov := range o {
			if _, ok := r[k]; !ok {
				d.add(join(path, k), "extra", nil, ov)
			}
		}
	case []any:
		o, ok := ours.([]any)
		if !ok {
			d.add(path, "value", ref, ours)
			return
		}
		d.array(path, r, o, geom)
	case float64:
		o, ok := ours.(float64)
		if !ok || !near(r, o, geom, d.quantum) {
			d.add(path, "value", ref, ours)
		}
	default:
		if ref != ours {
			d.add(path, "value", ref, ours)
		}
	}
}

func near(a, b float64, geom bool, quantum float64) bool {
	tol := 1e-9 * math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	if geom {
		tol = math.Max(tol, quantum)
	}
	return math.Abs(a-b) <= tol
}

// names keys an array of field descriptions by name, or reports false.
func names(arr []any) ([]string, map[string]any, bool) {
	keys := make([]string, 0, len(arr))
	by := map[string]any{}
	for _, v := range arr {
		m, _ := v.(map[string]any)
		n, ok := m["name"].(string)
		if !ok || m["type"] == nil {
			return nil, nil, false
		}
		keys = append(keys, n)
		by[n] = v
	}
	return keys, by, true
}

// rowKeys keys rows by object id, or reports false when a row has none or shares one.
func rowKeys(arr []any) (map[string]any, bool) {
	by := map[string]any{}
	for _, v := range arr {
		k := rowKey(v)
		if k == nil {
			return nil, false
		}
		if _, dup := by[show(k)]; dup {
			return nil, false
		}
		by[show(k)] = v
	}
	return by, true
}

func (d *differ) array(path string, ref, ours []any, geom bool) {
	if rn, rb, ok := names(ref); ok {
		if on, ob, ok := names(ours); ok {
			for _, n := range rn {
				if ov, ok := ob[n]; ok {
					d.walk(path+"["+n+"]", rb[n], ov, geom)
				} else {
					d.add(path+"["+n+"]", "missing", rb[n], nil)
				}
			}
			for _, n := range on {
				if _, ok := rb[n]; !ok {
					d.add(path+"["+n+"]", "extra", nil, ob[n])
				}
			}
			common := func(names []string, other map[string]any) []string {
				return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return other[n] == nil })
			}
			if !slices.Equal(common(rn, ob), common(on, rb)) {
				d.add(path, "order", rn, on)
			}
			return
		}
	}
	if len(ref) != len(ours) {
		d.add(path, "length", len(ref), len(ours))
	}
	if !d.ordered || len(ref) != len(ours) {
		if rb, ok := rowKeys(ref); ok {
			if ob, ok := rowKeys(ours); ok {
				for k, rv := range rb {
					if ov, ok := ob[k]; ok {
						d.walk(path+"[]", rv, ov, geom)
					} else {
						d.add(path+"[]", "missing", rv, nil)
					}
				}
				for k, ov := range ob {
					if _, ok := rb[k]; !ok {
						d.add(path+"[]", "extra", nil, ov)
					}
				}
				return
			}
		}
	}
	if len(ref) != len(ours) {
		return
	}
	for i := range ref {
		d.walk(path+"[]", ref[i], ours[i], geom)
	}
}

// Gap is an accepted difference. Each pattern in Match is matched against
// "layer/request: path (kind)", where * matches any text.
type Gap struct {
	Match  []string `json:"match"`
	Reason string   `json:"reason"`
	res    []*regexp.Regexp
}

// Gaps are the accepted differences. Ignore lists what never matters (free text, the hosting,
// capabilities we do not claim); Known lists what we do differently today, and each of its
// patterns must still match a difference.
type Gaps struct {
	Ignore []*Gap `json:"ignore"`
	Known  []*Gap `json:"known"`
}

// LoadGaps reads a gaps file.
func LoadGaps(path string) (*Gaps, error) {
	var g Gaps
	if err := ReadJSON(path, &g); err != nil {
		return nil, err
	}
	for _, x := range slices.Concat(g.Ignore, g.Known) {
		if x.Reason == "" || len(x.Match) == 0 {
			return nil, fmt.Errorf("%s: %q needs a pattern and a reason", path, x.Match)
		}
		for _, m := range x.Match {
			parts := strings.Split(m, "*")
			for i, p := range parts {
				parts[i] = regexp.QuoteMeta(p)
			}
			x.res = append(x.res, regexp.MustCompile("^"+strings.Join(parts, ".*")+"$"))
		}
	}
	return &g, nil
}

// Matches lists the indexes of the gap's patterns that match a key.
func (g *Gap) Matches(key string) []int {
	var out []int
	for i, re := range g.res {
		if re.MatchString(key) {
			out = append(out, i)
		}
	}
	return out
}

// Key is what a gap's patterns are matched against.
func Key(layer, request string, d Diff) string {
	return fmt.Sprintf("%s/%s: %s", layer, request, d)
}
