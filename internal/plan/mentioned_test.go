package plan

import (
	"regexp"
	"testing"
)

// mentioned agrees with the pattern it replaces.
func TestMentionedMatchesPattern(t *testing.T) {
	texts := []string{"", "1=1", "UnitSymbol = 'Qa'", "unitsymbol_x=1", "x_unitsymbol=1", "(UNITSYMBOL)", "a.unitsymbol>2",
		"unitsymbolunitsymbol", "unit symbol", "wetland_type IN ('a')", "  ", "é unitsymbol é", "feature_id asc", "9unitsymbol"}
	words := []string{"unitsymbol", "wetland_type", "feature_id", "a", "", "UnitSymbol", "x"}
	for _, s := range texts {
		for _, w := range words {
			want := regexp.MustCompile(`(?i)\b` + w + `\b`).MatchString(s)
			if got := mentioned(s, w); got != want {
				t.Errorf("mentioned(%q, %q) = %v, the pattern says %v", s, w, got, want)
			}
		}
	}
}
