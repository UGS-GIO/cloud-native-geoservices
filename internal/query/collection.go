package query

import (
	"strings"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
)

// Feature is one provider feature: its property values (aligned with Collection.Props) and its
// lon/lat geometry, nil when it has none.
type Feature struct {
	Props []any
	Geom  orb.Geometry
	Known bool // the provider found from its bbox that it meets the geometry filter
}

// Collection is what the provider hands Koop.
type Collection struct {
	Meta           *esri.Metadata
	Props          []string
	Features       []Feature
	Exceeded       Tri
	FiltersApplied map[string]bool // offset, limit, resultRecordCount, orderByFields
	Count          *int64          // precalculated: the file's row count
	Extent         jsonx.Obj       // precalculated: the file's extent
	// Next, when set, returns the features after these a chunk at a time, nil after the last.
	Next func() ([]Feature, error)
}

// field finds a layer field by name in any case: its name, and its index in Props (-1 if not read).
func (c *Collection) field(name string) (string, int, bool) {
	for _, f := range c.Meta.Fields {
		if strings.EqualFold(f.Name, name) {
			return f.Name, c.propIndex(f.Name), true
		}
	}
	return "", -1, false
}

func (c *Collection) propIndex(name string) int {
	for i, p := range c.Props {
		if p == name {
			return i
		}
	}
	return -1
}

// Tri is a JavaScript boolean that may also be null: the provider's exceededTransferLimit is null
// when it did no paging and nothing overflowed, and Koop passes that through.
type Tri int8

const (
	False Tri = iota
	True
	Null
)

// AppendJSON implements jsonx.Appender.
func (t Tri) AppendJSON(buf []byte) []byte {
	switch t {
	case True:
		return append(buf, "true"...)
	case Null:
		return append(buf, "null"...)
	}
	return append(buf, "false"...)
}
