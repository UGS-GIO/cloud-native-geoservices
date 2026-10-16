package pq

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T, name string) *Layer {
	path, _ := filepath.Abs("../../testdata/" + name)
	l, err := NewReader(16<<20, 16<<20, 64<<20).Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestOpen(t *testing.T) {
	l := open(t, "qfaults.parquet")
	if l.Rows != 300 || len(l.Groups) != 3 || l.GeometryType != "LineString" || !l.IDIsRow || !l.HasBBox || l.Extent == nil {
		t.Fatalf("%+v", l)
	}
	for _, c := range l.Columns {
		if internal[c.Name] || c.Name == l.Geom {
			t.Errorf("%s is not an attribute", c.Name)
		}
	}
}

func TestSelectAndRead(t *testing.T) {
	ctx := context.Background()
	l := open(t, "wells.parquet")
	all, _ := l.Select(ctx, Selection{Box: l.Extent})
	if len(all) != 300 {
		t.Errorf("the extent selects every row: %d", len(all))
	}
	page, _ := l.Select(ctx, Selection{Page: &Page{Offset: 290, Limit: 20}})
	if len(page) != 10 || page[0] != 290 {
		t.Errorf("last page %v", page)
	}
	ids, _ := l.Select(ctx, Selection{IDs: []int64{300, 1, 1, 999}})
	if len(ids) != 2 || ids[0] != 0 || ids[1] != 299 {
		t.Errorf("ids %v", ids)
	}
	rows, err := l.Read(ctx, []string{"feature_id", l.Geom}, []int64{0, 150, 299})
	if err != nil {
		t.Fatal(err)
	}
	if rows.Get(0, 1) != int64(151) || len(rows.Bytes(1, 2)) == 0 {
		t.Errorf("read %v", rows.Get(0, 1))
	}
	if _, err := l.Read(ctx, []string{"feature_id"}, []int64{300}); err == nil {
		t.Error("a row past the end should fail")
	}
}
