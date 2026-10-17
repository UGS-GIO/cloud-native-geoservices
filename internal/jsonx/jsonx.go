// Package jsonx writes JSON the way JavaScript's JSON.stringify does: object keys keep their
// insertion order, numbers print like Number#toString, and undefined values are left out.
package jsonx

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"
)

// KV is one key of an ordered object.
type KV struct {
	K string
	V any
}

// Obj is a JSON object whose keys print in insertion order.
type Obj []KV

// Undefined is a value JSON.stringify drops from objects (and prints as null in arrays).
type undefined struct{}

var Undefined = undefined{}

// IsUndefined reports the Undefined value.
func IsUndefined(v any) bool { _, ok := v.(undefined); return ok }

// Raw is already-encoded JSON, written as is.
type Raw = json.RawMessage

// Set replaces k's value, or appends k.
func (o *Obj) Set(k string, v any) {
	for i := range *o {
		if (*o)[i].K == k {
			(*o)[i].V = v
			return
		}
	}
	*o = append(*o, KV{k, v})
}

// Get returns k's value.
func (o Obj) Get(k string) (any, bool) {
	for _, kv := range o {
		if kv.K == k {
			return kv.V, true
		}
	}
	return nil, false
}

// Marshal encodes v.
func Marshal(v any) []byte { return Append(nil, v) }

// Appender is implemented by values that encode themselves.
type Appender interface{ AppendJSON([]byte) []byte }

// Append encodes v onto buf.
func Append(buf []byte, v any) []byte {
	switch x := v.(type) {
	case nil, undefined:
		return append(buf, "null"...)
	case bool:
		return strconv.AppendBool(buf, x)
	case int:
		return strconv.AppendInt(buf, int64(x), 10)
	case int64:
		if x > 1<<53 || x < -(1<<53) {
			return AppendNumber(buf, float64(x))
		}
		return strconv.AppendInt(buf, x, 10)
	case uint32:
		return strconv.AppendUint(buf, uint64(x), 10)
	case float64:
		return AppendNumber(buf, x)
	case string:
		return AppendString(buf, x)
	case Raw:
		if len(x) == 0 {
			return append(buf, "null"...)
		}
		return append(buf, x...)
	case Appender:
		return x.AppendJSON(buf)
	case Obj:
		buf = append(buf, '{')
		first := true
		for _, kv := range x {
			if _, skip := kv.V.(undefined); skip {
				continue
			}
			if !first {
				buf = append(buf, ',')
			}
			first = false
			buf = AppendString(buf, kv.K)
			buf = append(buf, ':')
			buf = Append(buf, kv.V)
		}
		return append(buf, '}')
	case []any:
		return AppendArray(buf, x, Append)
	case []Obj:
		return AppendArray(buf, x, appendAny[Obj])
	case []string:
		return AppendArray(buf, x, AppendString)
	case []int64:
		return AppendArray(buf, x, appendAny[int64])
	case []float64:
		return AppendArray(buf, x, AppendNumber)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return append(buf, "null"...)
		}
		return append(buf, b...)
	}
}

func appendAny[T any](buf []byte, v T) []byte { return Append(buf, v) }

// AppendArray writes xs as a JSON array, each element with f.
func AppendArray[T any](buf []byte, xs []T, f func([]byte, T) []byte) []byte {
	buf = append(buf, '[')
	for i, x := range xs {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = f(buf, x)
	}
	return append(buf, ']')
}

// AppendNumber prints f like JavaScript's Number#toString.
func AppendNumber(buf []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(buf, "null"...)
	}
	if f == 0 {
		return append(buf, '0')
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		start := len(buf)
		buf = strconv.AppendFloat(buf, f, 'e', -1, 64)
		// Go writes e-07 and e+21; JavaScript writes e-7 and e+21.
		for i := start; i < len(buf); i++ {
			if buf[i] == 'e' && i+2 < len(buf) && buf[i+2] == '0' {
				buf = append(buf[:i+2], buf[i+3:]...)
				break
			}
		}
		return buf
	}
	return strconv.AppendFloat(buf, f, 'f', -1, 64)
}

// NumberString is AppendNumber as a string, for keys built from values.
func NumberString(f float64) string { return string(AppendNumber(nil, f)) }

const hex = "0123456789abcdef"

// AppendString quotes s as JSON.stringify does (only quotes, backslashes and controls escaped).
func AppendString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			buf = append(buf, s[start:i]...)
			switch c {
			case '"':
				buf = append(buf, '\\', '"')
			case '\\':
				buf = append(buf, '\\', '\\')
			case '\n':
				buf = append(buf, '\\', 'n')
			case '\r':
				buf = append(buf, '\\', 'r')
			case '\t':
				buf = append(buf, '\\', 't')
			case '\b':
				buf = append(buf, '\\', 'b')
			case '\f':
				buf = append(buf, '\\', 'f')
			default:
				buf = append(buf, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf = append(buf, s[start:i]...)
			buf = append(buf, "�"...)
			i += size
			start = i
			continue
		}
		i += size
	}
	buf = append(buf, s[start:]...)
	return append(buf, '"')
}

// Parse decodes JSON keeping object key order: objects become Obj, arrays []any, numbers float64.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return parseValue(dec)
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			o := Obj{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o = append(o, KV{k.(string), v})
			}
			_, err := dec.Token()
			return o, err
		}
		a := []any{}
		for dec.More() {
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	case json.Number:
		return t.Float64()
	default:
		return t, nil
	}
}
