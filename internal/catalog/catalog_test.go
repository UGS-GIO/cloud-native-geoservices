package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaleOnError(t *testing.T) {
	c := NewTTL(10 * time.Millisecond)
	c.Get("k", func() (any, error) { return 1, nil })
	time.Sleep(20 * time.Millisecond)
	v, err := c.Get("k", func() (any, error) { return nil, errors.New("cdn down") })
	if err != nil || v != 1 {
		t.Fatalf("stale value not served: %v %v", v, err)
	}
	asked := false
	c.Get("k", func() (any, error) { asked = true; return nil, errors.New("cdn down") })
	if asked {
		t.Fatal("a failed refresh should not be retried on the very next request")
	}
	if _, err := c.Get("other", func() (any, error) { return nil, errors.New("down") }); err == nil {
		t.Fatal("no stale value: the error should surface")
	}
}

func TestParseTopics(t *testing.T) {
	doc := `{"items":[
	 {"id":"a","properties":{"title":"A","ugs:renders":{"z":{},"b":{"esri_url":"https://x/b.json","title":"B"}}},"assets":{"data":{"href":"a.parquet"},"overview_1":{"href":"o.parquet","ugs:simplify_tolerance":0.01}}},
	 {"id":"no-data","assets":{"thumbnail":{"href":"t.webp"}}},
	 {"id":"c","properties":{},"assets":{"data":{"href":"c.parquet"}}}]}`
	items, err := parseSTAC([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	tp := index(items)
	if len(tp.order) != 2 || tp.order[0].ID != "a" {
		t.Fatalf("topics %v", tp.order)
	}
	a := tp.byID["a"]
	if a.Renders[0].Name != "z" || a.Renders[1].EsriURL != "https://x/b.json" || *a.Title != "A" || len(a.Assets) != 2 {
		t.Errorf("renders keep document order: %+v", a)
	}
	if c := tp.byID["c"]; len(c.Renders) != 1 || c.Renders[0].Name != "default" || c.Title != nil {
		t.Errorf("a topic without renders is served as default: %+v", c)
	}
}

func TestRendererVersionFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	esri := filepath.Join(dir, "b.json")
	items := filepath.Join(dir, "items.json")
	os.WriteFile(items, []byte(`{"items":[{"id":"a","properties":{"ugs:renders":{"b":{"esri_url":"`+esri+`"}}},"assets":{"data":{"href":"a.parquet"}}}]}`), 0o644)
	c := New(STAC{URL: items}, 0)
	version := func(renderer string) string {
		os.WriteFile(esri, []byte(`{"renderer":`+renderer+`}`), 0o644)
		it, err := c.Topic(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		d, err := c.Drawing(context.Background(), it, "b")
		if err != nil {
			t.Fatal(err)
		}
		return d.Version
	}
	if version(`{"type":"simple"}`) == version(`{"type":"uniqueValue"}`) {
		t.Fatal("a republished renderer kept its version")
	}
}

func TestMetadataReadsPastTheCDN(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		if r.URL.Path == "/items.json" {
			w.Write([]byte(`{"items":[{"id":"a","properties":{"ugs:renders":{"b":{"esri_url":"b.json?x=1"}}},"assets":{"data":{"href":"a.parquet"}}}]}`))
			return
		}
		w.Write([]byte(`{"renderer":{}}`))
	}))
	defer srv.Close()
	c := New(STAC{URL: srv.URL + "/items.json"}, time.Minute)
	it, err := c.Topic(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	it.Renders[0].EsriURL = srv.URL + "/" + it.Renders[0].EsriURL
	if _, err := c.Drawing(context.Background(), it, "b"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "/items.json?_cb=") || !strings.HasPrefix(got[1], "/b.json?x=1&_cb=") {
		t.Fatalf("each metadata read needs a _cb parameter: %q", got)
	}
}

func TestFolder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.parquet", "a.parquet", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o644)
	}
	os.Mkdir(filepath.Join(dir, "sub.parquet"), 0o755)
	items, err := Folder{Dir: dir}.Items(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "a" || items[0].Data() != filepath.Join(dir, "a.parquet") {
		t.Fatalf("one topic per .parquet file, by name: %+v", items)
	}
	if _, err := (Folder{Dir: filepath.Join(dir, "missing")}).Items(context.Background(), nil); err == nil {
		t.Fatal("a missing folder should be an error")
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layers.json")
	os.WriteFile(path, []byte(`{"layers":[
	 {"id":"faults","title":"Faults","url":"data/faults.parquet",
	  "renders":[{"name":"by-age","title":"By age","esri_url":"styles/age.json"},{"name":"plain"}]},
	 {"id":"wells","url":"https://example.com/wells.parquet"}]}`), 0o644)
	c := New(Config{URL: path}, 0)
	f, err := c.Topic(context.Background(), "faults")
	if err != nil {
		t.Fatal(err)
	}
	if f.Data() != filepath.Join(dir, "data/faults.parquet") || *f.Title != "Faults" ||
		f.Renders[0].EsriURL != filepath.Join(dir, "styles/age.json") || f.Renders[1].Name != "plain" {
		t.Errorf("relative urls resolve against the config file: %+v", f)
	}
	w, _ := c.Topic(context.Background(), "wells")
	if w.Data() != "https://example.com/wells.parquet" || w.Renders[0].Name != "default" {
		t.Errorf("a layer without renders is served as default: %+v", w)
	}
	if got := resolve("https://cdn.example.com/cfg/layers.json", "a.parquet"); got != "https://cdn.example.com/cfg/a.parquet" {
		t.Errorf("resolve against a URL: %s", got)
	}
	os.WriteFile(path, []byte(`{"layers":[{"id":"x"}]}`), 0o644)
	if _, err := (Config{URL: path}).Items(context.Background(), New(nil, 0).getBytes); err == nil {
		t.Error("a layer without a url should be an error")
	}
}
