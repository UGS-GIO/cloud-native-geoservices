package plan

import (
	"regexp"
	"testing"
)

// A column is mentioned where the pattern it replaces matches.
func TestMentionedMatchesPattern(t *testing.T) {
	texts := []string{"", "1=1", "UnitSymbol = 'Qa'", "unitsymbol_x=1", "x_unitsymbol=1", "(UNITSYMBOL)", "a.unitsymbol>2",
		"unitsymbolunitsymbol", "unit symbol", "wetland_type IN ('a')", "  ", "é unitsymbol é", "feature_id asc", "9unitsymbol"}
	names := []string{"unitsymbol", "wetland_type", "feature_id", "a", "", "UnitSymbol", "x", "unit symbol", "unit-symbol", "é"}
	nonWord := regexp.MustCompile(`[^\w]`)
	for _, s := range texts {
		for _, name := range names {
			want := regexp.MustCompile(`(?i)\b` + nonWord.ReplaceAllString(name, "") + `\b`).MatchString(s)
			if got := mentionedIn(words(s), name); got != want {
				t.Errorf("mentionedIn(words(%q), %q) = %v, the pattern says %v", s, name, got, want)
			}
		}
	}
}
