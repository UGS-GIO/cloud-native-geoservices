package where

import (
	"strings"
	"testing"
)

var fields = []string{"acres", "wetland_type", "name", "count", "flag", "date", "timestamp"}

func resolve(name string) (int, bool) {
	for i, f := range fields {
		if strings.EqualFold(f, name) {
			return i, true
		}
	}
	return 0, false
}

func row(vals ...any) Row { return func(i int) any { return vals[i] } }

func TestMatch(t *testing.T) {
	lake := row(12.5, "Lake", "Utah Lake", int64(3), true)
	null := row(nil, nil, "x", nil, false)
	tests := []struct {
		where string
		r     Row
		want  bool
	}{
		{"wetland_type = 'Lake'", lake, true},
		{"WETLAND_TYPE = 'lake'", lake, false},
		{"wetland_type IN ('Lake','Freshwater Pond') AND acres > 1", lake, true},
		{"wetland_type NOT IN ('Lake')", lake, false},
		{"wetland_type LIKE 'la%'", lake, true},
		{"wetland_type LIKE 'L_ke'", lake, true},
		{"name NOT LIKE '%Lake'", lake, false},
		{"acres BETWEEN 10 AND 13", lake, true},
		{"acres NOT BETWEEN 10 AND 13", lake, false},
		{"acres >= 12.5 AND count < 4", lake, true},
		{"acres <> 12.5 OR count = 3", lake, true},
		{"NOT (acres > 100)", lake, true},
		{"acres IS NULL", null, true},
		{"acres IS NOT NULL", lake, true},
		{"acres > 1", null, false},
		{"NOT (acres > 1)", null, true}, // alasql: a null comparison is falsy, and NOT of it true
		{"UPPER(wetland_type) = 'LAKE'", lake, true},
		{"lower(name) LIKE '%utah%'", lake, true},
		{"count = '3'", lake, false},  // = is strict
		{"count <> '3'", lake, false}, // <> is loose
		{"name = 'O''Brien'", row(nil, nil, "O'Brien", nil, nil), true},
		{"missing = 1", lake, false},
		{"acres > -1", lake, true},
		{`"wetland_type" = 'Lake'`, lake, true},
		{"date IN ('1961')", row(nil, nil, nil, nil, nil, "1961", nil), true},
		{"date = '1961' AND timestamp IS NULL", row(nil, nil, nil, nil, nil, "1961", nil), true},
		{"DATE IS NOT NULL", row(nil, nil, nil, nil, nil, "x", nil), true},
		{"timestamp > DATE '2020-01-01'", row(nil, nil, nil, nil, nil, nil, int64(1600000000000)), true},
		{"date LIKE '19%' ESCAPE '!'", row(nil, nil, nil, nil, nil, "1961", nil), true},
	}
	for _, tt := range tests {
		e, err := Parse(tt.where, resolve)
		if err != nil {
			t.Fatalf("%s: %v", tt.where, err)
		}
		if got := Match(e, tt.r); got != tt.want {
			t.Errorf("%s = %v, want %v", tt.where, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, w := range []string{"foo === ", "acres >", "(acres > 1", "acres IN 1", "'open", "acres BETWEEN 1", "x LIKE"} {
		if _, err := Parse(w, resolve); err == nil {
			t.Errorf("%q parsed", w)
		} else if !strings.Contains(err.Error(), `Invalid "where" parameter`) {
			t.Errorf("%q: %v", w, err)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"1=1", "wetland_type = 'Lake'", "wetland_type IN ('Lake','Freshwater Pond') AND acres > 1",
		"wetland_type LIKE 'Fresh%' AND acres >= 500", "NOT (a IS NULL) OR b BETWEEN 1 AND 2", "UPPER(x) = 'A'", "DATE '2020-01-01' < x", "date IN ('1961')", "timestamp = date"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := Parse(s, resolve)
		if err != nil {
			return
		}
		Match(e, row(1.5, "a", "b", int64(2), true, "1961", int64(0)))
		Match(e, row(nil, nil, nil, nil, nil, nil, nil))
	})
}
