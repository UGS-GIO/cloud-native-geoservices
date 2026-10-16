package js

import (
	"math"
	"testing"
)

func TestNumber(t *testing.T) {
	for in, want := range map[string]float64{"": 0, " 5 ": 5, "0x10": 16, "1e3": 1000, "-Infinity": math.Inf(-1)} {
		if got := ParseNumber(in); got != want {
			t.Errorf("Number(%q) = %v", in, got)
		}
	}
	for _, in := range []string{"abc", "1_000", "inf", "NaN", "1,2"} {
		if !math.IsNaN(ParseNumber(in)) {
			t.Errorf("Number(%q) should be NaN", in)
		}
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name string
		got  bool
	}{
		{"1 === 1.0", StrictEqual(int64(1), 1.0)},
		{"'1' !== 1", !StrictEqual("1", 1.0)},
		{"'1' == 1", LooseEqual("1", int64(1))},
		{"'b' < 'c'", Less("b", "c")},
		{"'10' < 9 is false (numeric)", !Less("10", 9.0)},
		{"'abc' <= 1 is false", !LessEqual("abc", 1.0)},
		{"true == 1", LooseEqual(true, 1.0)},
		{"String(null)", String(nil) == "null" && Join(nil) == ""},
		{"String(1e21)", String(1e21) == "1e+21"},
	}
	for _, tt := range tests {
		if !tt.got {
			t.Error(tt.name)
		}
	}
}
