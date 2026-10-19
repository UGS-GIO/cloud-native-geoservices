package where

import (
	"strings"
	"testing"
)

var fields = [][2]string{{"acres", "Double"}, {"wetland_type", "String"}, {"name", "String"},
	{"count", "Integer"}, {"flag", "Integer"}, {"date", "String"}, {"timestamp", "Date"}}

func resolve(name string) (int, string, bool) {
	for i, f := range fields {
		if strings.EqualFold(f[0], name) {
			return i, f[1], true
		}
	}
	return 0, "", false
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
		{"WETLAND_TYPE = 'lake'", lake, true}, // strings ignore case
		{"wetland_type <> 'LAKE'", lake, false},
		{"wetland_type IN ('lake')", lake, true},
		{"wetland_type > 'kz'", lake, true},
		{"name BETWEEN 'UTAHL' AND 'UTAHM' AND name <> 'utahlake'", row(nil, nil, "Utah-Lake"), true}, // word sort
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
		{"NOT (acres > 1)", null, false}, // NOT unknown is unknown
		{"acres > 1 OR NOT (acres > 1)", null, false},
		{"acres > 1 OR acres IS NULL", null, true},
		{"NOT (acres > 1 AND count = 4)", lake, true},
		{"wetland_type NOT IN ('x', NULL)", lake, false},
		{"wetland_type IN ('Lake', NULL)", lake, true},
		{"UPPER(wetland_type) = 'LAKE'", lake, true},
		{"lower(name) LIKE '%utah%'", lake, true},
		{"count = '3'", lake, true}, // a literal converts to the field's type
		{"count <> ' 3 '", lake, false},
		{"count IN ('3', 4)", lake, true},
		{"acres > '12.25'", lake, true},
		{"name = 5", lake, false},
		{"name <> 5", lake, true},
		{"1 = '1'", lake, true},
		{"TRUE", lake, true},
		{"count LIKE '3%'", lake, true},
		{"date > 1900 AND date IN (1961, '1962')", row(nil, nil, nil, nil, nil, " 1961", nil), true}, // the string converts
		{"name = 'x' AND name > 5", lake, false},
		{"acres + 1 = 13.5 AND acres = 10 + 2.5", lake, true},
		{"count * 2 - 1 = 5 AND count / 2 = 1 AND acres / 5 = 2.5", lake, true}, // integers divide to an integer
		{"wetland_type + 's' = 'lakes' AND name || '!' = 'Utah Lake!'", lake, true},
		{"date + 1 = 1962", row(nil, nil, nil, nil, nil, "1961", nil), true},
		{"acres + 1 > 0", null, false},
		{"timestamp > '2020-01-01'", row(nil, nil, nil, nil, nil, nil, int64(1600000000000)), true},
		{"name = 'O''Brien'", row(nil, nil, "O'Brien", nil, nil), true},
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
		if got := e.Match(tt.r); got != tt.want || e.Err() != nil {
			t.Errorf("%s = %v, want %v", tt.where, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, w := range []string{"foo === ", "acres >", "(acres > 1", "acres IN 1", "'open", "acres BETWEEN 1", "x LIKE", "missing = 1", "missing IS NULL",
		"timestamp = 0", "timestamp > 'x'", "count = '3.5'", "count = TRUE", "count",
		"NOT name", "1", "acres > 1 AND name",
		"count / 0 = 1", "timestamp + 1 > 0", "name - 'a' = 'b'", "count || 'x' = 'y'"} {
		if _, err := Parse(w, resolve); err == nil {
			t.Errorf("%q parsed", w)
		} else if !strings.Contains(err.Error(), `Invalid "where" parameter`) {
			t.Errorf("%q: %v", w, err)
		}
	}
}

func TestConversionError(t *testing.T) {
	for _, w := range []string{"name > 5", "name IN (5)", "acres > 1 AND name BETWEEN 1 AND 2", "name + 1 = 2", "acres / (count - 3) > 1"} {
		e, err := Parse(w, resolve)
		if err != nil {
			t.Fatalf("%s: %v", w, err)
		}
		if e.Match(row(12.5, "Lake", "Utah Lake", int64(3), true)) || e.Err() == nil {
			t.Errorf("%s: no error converting a name", w)
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
		e.Match(row(1.5, "a", "b", int64(2), true, "1961", int64(0)))
		e.Match(row(nil, nil, nil, nil, nil, nil, nil))
	})
}
