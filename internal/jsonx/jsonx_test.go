package jsonx

import "testing"

func TestAppend(t *testing.T) {
	tests := []struct {
		v    any
		want string
	}{
		{Obj{{"b", 1}, {"a", Undefined}, {"c", "x\"\n<"}}, `{"b":1,"c":"x\"\n<"}`},
		{1e21, "1e+21"},
		{1.5e-7, "1.5e-7"},
		{0.30000000000000004, "0.30000000000000004"},
		{int64(1) << 60, "1152921504606847000"}, // past 2^53, as JavaScript prints the Number
		{[]any{nil, true, 2.5}, "[null,true,2.5]"},
		{"\x01", `"\u0001"`},
	}
	for _, tt := range tests {
		if got := string(Marshal(tt.v)); got != tt.want {
			t.Errorf("%v: %s, want %s", tt.v, got, tt.want)
		}
	}
	v, err := Parse([]byte(`{"z":1,"a":[{"y":2,"b":null}]}`))
	if err != nil || string(Marshal(v)) != `{"z":1,"a":[{"y":2,"b":null}]}` {
		t.Errorf("parse keeps key order: %s %v", Marshal(v), err)
	}
}
