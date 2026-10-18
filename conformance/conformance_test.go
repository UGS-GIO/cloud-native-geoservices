package conformance

import (
	"os"
	"testing"
)

const testdata = "../testdata/conformance"

func layers(t *testing.T) Layers {
	var l Layers
	if err := ReadJSON(testdata+"/layers.json", &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestSmall compares the committed small layers with their recorded Esri answers.
func TestSmall(t *testing.T) {
	check(t, testdata+"/layers", testdata+"/known_gaps.json", layers(t).Small)
}

// TestLarge compares the large layers in $CONFORMANCE_LARGE (README.md) and logs timings.
func TestLarge(t *testing.T) {
	dir := os.Getenv("CONFORMANCE_LARGE")
	if dir == "" {
		t.Skip("set CONFORMANCE_LARGE to the directory of the large layers")
	}
	for _, tm := range check(t, dir, testdata+"/known_gaps_large.json", layers(t).Large) {
		t.Logf("%-40s esri %8s  ours %8s", tm.name, tm.esri, tm.ours)
	}
}
