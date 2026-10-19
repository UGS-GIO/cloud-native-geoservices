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

func TestString(t *testing.T) {
	if String(nil) != "null" || Join(nil) != "" || String(1e21) != "1e+21" {
		t.Error("String")
	}
}
