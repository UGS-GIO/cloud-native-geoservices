// Package catalog lists what is served and how it looks, from a Source, on demand.
//
// A topic is one GeoParquet file. Each of its renders is its own FeatureServer, symbolized by the
// Esri drawing info at the render's `esri_url`. A topic with no renders is served once, as
// `default`, with the default symbol.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Error carries the Esri error code a lookup failed with.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Field is one key of a JSON object, in document order.
type Field struct {
	Key   string
	Value json.RawMessage
}

// Asset is one STAC asset.
type Asset struct {
	Key       string
	Href      string
	Tolerance *float64 // ugs:simplify_tolerance, on overview assets
}

// Render is one way to draw a topic.
type Render struct {
	Name    string  `json:"name"`
	Title   *string `json:"title"`
	EsriURL string  `json:"esri_url"`
}

// Item is a served topic.
type Item struct {
	ID          string
	Title       *string // properties.title, when set
	Description string
	Assets      []Asset
	Renders     []Render // never empty: a topic without renders has `default`
	Version     string   // changes whenever the item's JSON does
}

// Data is the GeoParquet asset's href.
func (it *Item) Data() string {
	for _, a := range it.Assets {
		if a.Key == "data" {
			return a.Href
		}
	}
	return ""
}

// Catalog caches the topics a Source lists and the renderers they point to.
type Catalog struct {
	Source Source
	Cache  *TTL
	HTTP   *http.Client
}

type topics struct {
	order []*Item
	byID  map[string]*Item
}

// New makes a catalog over src.
func New(src Source, ttl time.Duration) *Catalog {
	return &Catalog{Source: src, Cache: NewTTL(ttl), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Topics lists every served topic, in collection order.
func (c *Catalog) Topics(ctx context.Context) ([]*Item, error) {
	t, err := c.topics(ctx)
	if err != nil {
		return nil, err
	}
	return t.order, nil
}

func (c *Catalog) topics(ctx context.Context) (*topics, error) {
	v, err := c.Cache.Get("topics", func() (any, error) {
		items, err := c.Source.Items(ctx, c.getBytes)
		if err != nil {
			return nil, err
		}
		return index(items), nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*topics), nil
}

// Topic is one topic by id.
func (c *Catalog) Topic(ctx context.Context, id string) (*Item, error) {
	t, err := c.topics(ctx)
	if err != nil {
		return nil, err
	}
	it, ok := t.byID[id]
	if !ok {
		return nil, &Error{404, "unknown topic: " + id}
	}
	return it, nil
}

// Drawing is the Esri drawing info for one render.
type Drawing struct {
	// Info is renderer, labelingInfo, minScale (raw JSON); empty for a render with no esri_url.
	Info map[string]json.RawMessage
	// Version changes when the render's Esri JSON does, though its URL stays the same.
	Version string
}

// Drawing is one render's drawing info.
func (c *Catalog) Drawing(ctx context.Context, it *Item, name string) (*Drawing, error) {
	var r *Render
	names := make([]string, len(it.Renders))
	for i := range it.Renders {
		names[i] = it.Renders[i].Name
		if it.Renders[i].Name == name {
			r = &it.Renders[i]
		}
	}
	if r == nil {
		return nil, &Error{404, fmt.Sprintf("unknown render '%s' for %s (have: %s)", name, it.ID, strings.Join(names, ", "))}
	}
	if r.EsriURL == "" {
		return &Drawing{Info: map[string]json.RawMessage{}}, nil
	}
	v, err := c.Cache.Get("esri:"+r.EsriURL, func() (any, error) {
		raw, err := c.getBytes(ctx, r.EsriURL)
		if err != nil {
			return nil, err
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		return &Drawing{doc, hash(raw)}, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Drawing), nil
}

// getBytes reads a URL or local file.
func (c *Catalog) getBytes(ctx context.Context, url string) ([]byte, error) {
	if !isHTTP(url) {
		return os.ReadFile(url)
	}
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &Error{res.StatusCode, fmt.Sprintf("%d for %s", res.StatusCode, url)}
	}
	return io.ReadAll(res.Body)
}

// ObjectFields splits a JSON object into its keys, in document order.
func ObjectFields(raw json.RawMessage) ([]Field, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil
	}
	var out []Field
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, Field{k.(string), v})
	}
	return out, nil
}

// index keeps the last item of each id, in the place of the first.
func index(items []*Item) *topics {
	t := &topics{byID: map[string]*Item{}}
	for _, it := range items {
		if len(it.Renders) == 0 {
			it.Renders = []Render{{Name: "default"}}
		}
		if prev, ok := t.byID[it.ID]; ok {
			t.order[slices.Index(t.order, prev)] = it
		} else {
			t.order = append(t.order, it)
		}
		t.byID[it.ID] = it
	}
	return t
}
