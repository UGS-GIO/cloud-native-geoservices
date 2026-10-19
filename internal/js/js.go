// Package js gives values JavaScript's number and string conversions, which parameter parsing and
// the PBF encoder use: values are nil, bool, int64, float64 or string.
package js

import (
	"math"
	"strconv"
	"strings"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
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
