package geom

import (
	"testing"

	"github.com/paulmach/orb"
)

func TestRound(t *testing.T) {
	for in, want := range map[float64]int64{2.5: 3, -2.5: -2, -2.6: -3, 0.49999999999999994: 0, 1.5: 2} {
		if got := Round(in); got != want {
			t.Errorf("Round(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestAppendQuantized(t *testing.T) {
	ul := &Transform{XScale: 10, YScale: 10, YTranslate: 100}
	ll := &Transform{XScale: 10, YScale: 10, LowerLeft: true}
	line := Esri{Kind: EsriPolyline, Parts: [][]orb.Point{{{4, 96}, {3, 97}, {24, 76}}, {{50, 50}, {51, 51}}}}
	var scratch []int64
	tests := []struct {
		e    Esri
		t    *Transform
		want string
		ok   bool
	}{
		// Deltas after the first point; the second point rounds onto the first and is dropped,
		// and the second path collapses.
		{line, ul, `{"paths":[[[0,0],[2,2]]]}`, true},
		// Lower left counts y up and truncates.
		{line, ll, `{"paths":[[[0,9],[2,-2]]]}`, true},
		{Esri{Kind: EsriPolygon, Parts: [][]orb.Point{{{0, 0}, {1, 1}, {0, 0}}}}, ul, `{"rings":[]}`, false},
		{Esri{Kind: EsriPoint, X: 26, Y: 64}, ul, `{"x":3,"y":4}`, true},
		{Esri{Kind: EsriMultipoint, Parts: [][]orb.Point{{{0, 100}, {20, 80}}}}, ul, `{"points":[[0,0],[2,2]]}`, true},
	}
	for _, tt := range tests {
		got, ok := tt.e.AppendQuantized(nil, tt.t, &scratch)
		if string(got) != tt.want || ok != tt.ok {
			t.Errorf("%+v: got %s %v, want %s %v", tt.e, got, ok, tt.want, tt.ok)
		}
	}
	if x, y := ul.Centroid(&line); x != 3 || y != 3 {
		t.Errorf("centroid %d,%d", x, y)
	}
	edit := &Transform{XScale: 1e-4, YScale: 1e-4, XTranslate: -20037700, YTranslate: -30241100, Edit: true}
	if got := string(edit.AppendJSON(nil)); got != `{"originPosition":"upperLeft","scale":[0.0001,0.0001,0,0],"translate":[-20037700,-30241100,0,0]}` {
		t.Error(got)
	}
}
