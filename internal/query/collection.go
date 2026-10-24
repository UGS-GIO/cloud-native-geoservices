package query

import (
	"strings"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/esri"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/jsonx"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/pq"
)

// Feature is one provider feature: its property values (aligned with Collection.Props) and its
// lon/lat geometry, nil when it has none.
type Feature struct {
	Props []any
	Geom  orb.Geometry
	Known bool // the provider found from its bbox that it meets the geometry filter
}

// Collection is what the provider hands a query.
type Collection struct {
	Meta           *esri.Metadata
	Props          []string
	Features       []Feature
	Exceeded       bool
	PrecisionMM    float64         // JSON coordinates round to about this without geometryPrecision; 0 keeps them
	FiltersApplied map[string]bool // offset, limit, resultRecordCount, orderByFields
	Count          *int64          // precalculated: the file's row count
	Extent         jsonx.Obj       // precalculated: the file's extent
	// Next, when set, returns the features after these a chunk at a time, nil after the last.
	Next func() ([]Feature, error)
}

// field finds a layer field by name in any case, and its index in Props (-1 if not read).
func (c *Collection) field(name string) (pq.Column, int, bool) {
	for _, f := range c.Meta.Fields {
		if strings.EqualFold(f.Name, name) {
			return f, c.propIndex(f.Name), true
		}
	}
	return pq.Column{}, -1, false
}

func (c *Collection) propIndex(name string) int {
	for i, p := range c.Props {
		if p == name {
			return i
		}
	}
	return -1
}
