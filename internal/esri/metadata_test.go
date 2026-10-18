package esri

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
)

func TestScaleRange(t *testing.T) {
	m := &Metadata{Name: "x", GeometryType: "Polygon", MinScale: json.RawMessage("1000000"), MaxScale: json.RawMessage("5000")}
	layers, _ := Server(m).Get("layers")
	for doc, o := range map[string]jsonx.Obj{"layer": Layer(m, "0"), "server": layers.([]any)[0].(jsonx.Obj)} {
		if b := jsonx.Marshal(o); !strings.Contains(string(b), `"maxScale":5000`) {
			t.Errorf("%s maxScale missing: %s", doc, b)
		}
	}
	if b := jsonx.Marshal(Layer(&Metadata{Name: "x", GeometryType: "Polygon"}, "0")); !strings.Contains(string(b), `"maxScale":0`) {
		t.Errorf("unset maxScale: %s", b)
	}
}
