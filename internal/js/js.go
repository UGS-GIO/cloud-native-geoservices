// Package js gives values JavaScript's conversion and comparison rules, which Koop's responses
// depend on: values are nil, bool, int64, float64 or string.
package js

import (
	"math"
	"strconv"
	"strings"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
)

// ParseNumber is Number(s) for a string: trimmed, "" is 0, 0x hex, Infinity; NaN otherwise.
func ParseNumber(s string) float64 {
	t := strings.TrimSpace(s)
	switch {
	case t == "":
		return 0
	case t == "Infinity" || t == "+Infinity":
		return math.Inf(1)
	case t == "-Infinity":
		return math.Inf(-1)
	case strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X"):
		if n, err := strconv.ParseUint(t[2:], 16, 64); err == nil {
			return float64(n)
		}
		return math.NaN()
	case strings.ContainsAny(t, "_iInN"): // Go accepts 1_000, inf and nan; JavaScript does not
		return math.NaN()
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f
	}
	return math.NaN()
}

// Number is Number(v).
func Number(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case int64:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		return ParseNumber(x)
	}
	return math.NaN()
}

// String is String(v); null prints "null".
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case int64:
		return jsonx.NumberString(float64(x))
	case float64:
		if math.IsNaN(x) {
			return "NaN"
		}
		return jsonx.NumberString(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// Join is how Array#join prints a value: null as "".
func Join(v any) string {
	if v == nil {
		return ""
	}
	return String(v)
}

// IsNullish is v == null, or NaN (which alasql also treats as missing).
func IsNullish(v any) bool {
	f, ok := v.(float64)
	return v == nil || ok && math.IsNaN(f)
}

// Truthy is Boolean(v).
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

func bothStrings(a, b any) (string, string, bool) {
	sa, ok1 := a.(string)
	sb, ok2 := b.(string)
	return sa, sb, ok1 && ok2
}

// StrictEqual is a === b (numbers of either Go type compare by value).
func StrictEqual(a, b any) bool {
	if sa, sb, ok := bothStrings(a, b); ok {
		return sa == sb
	}
	if ba, ok := a.(bool); ok {
		bb, ok := b.(bool)
		return ok && ba == bb
	}
	x, ok1 := AsNumber(a)
	y, ok2 := AsNumber(b)
	return ok1 && ok2 && x == y
}

// AsNumber is v when it is a number.
func AsNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// LooseEqual is a == b for non-null values.
func LooseEqual(a, b any) bool {
	if sa, sb, ok := bothStrings(a, b); ok {
		return sa == sb
	}
	return Number(a) == Number(b)
}

// Less is a < b.
func Less(a, b any) bool {
	if sa, sb, ok := bothStrings(a, b); ok {
		return sa < sb
	}
	return Number(a) < Number(b)
}

// LessEqual is a <= b.
func LessEqual(a, b any) bool {
	if sa, sb, ok := bothStrings(a, b); ok {
		return sa <= sb
	}
	return Number(a) <= Number(b)
}
