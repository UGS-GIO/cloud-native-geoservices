// Package catalog reads what is served and how it looks from the STAC catalog, on demand.
//
// A layer is a serving topic with a GeoParquet `data` asset; each of its `ugs:renders` is its own
// FeatureServer, symbolized by the Esri renderer ugs-styles publishes beside the GL style
// (`esri_url`). A topic with no renders is served once, as `default`, with Koop's symbol.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strconv"
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

// Render is one entry of `ugs:renders`.
type Render struct {
	Name    string
	Title   *string
	EsriURL string
}

// Item is a serving topic.
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

// Catalog fetches and caches the collection and the renderers it points to.
type Catalog struct {
	URL   string
	Cache *TTL
	HTTP  *http.Client
}

type topics struct {
	order []*Item
	byID  map[string]*Item
}

// New makes a catalog over the collection at url (https or a local file).
func New(url string, ttl time.Duration) *Catalog {
	return &Catalog{URL: url, Cache: NewTTL(ttl), HTTP: &http.Client{Timeout: 10 * time.Second}}
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
		raw, err := c.getBytes(ctx, c.URL)
		if err != nil {
			return nil, err
		}
		return parseTopics(raw)
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

// Renderer is the Esri drawing info for one render: renderer, labelingInfo, minScale (raw JSON),
// or an empty map for a render with no esri_url.
func (c *Catalog) Renderer(ctx context.Context, it *Item, name string) (map[string]json.RawMessage, error) {
	d, err := c.drawing(ctx, it, name)
	if err != nil {
		return nil, err
	}
	return d.info, nil
}

// RendererVersion changes when the render's Esri JSON does, though its URL stays the same.
func (c *Catalog) RendererVersion(ctx context.Context, it *Item, name string) (string, error) {
	d, err := c.drawing(ctx, it, name)
	if err != nil {
		return "", err
	}
	return d.version, nil
}

type drawing struct {
	info    map[string]json.RawMessage
	version string
}

func (c *Catalog) drawing(ctx context.Context, it *Item, name string) (*drawing, error) {
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
		return &drawing{info: map[string]json.RawMessage{}}, nil
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
		h := fnv.New64a()
		h.Write(raw)
		return &drawing{doc, strconv.FormatUint(h.Sum64(), 36)}, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*drawing), nil
}

// GetBytes reads a URL or local file.
func (c *Catalog) getBytes(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "http:") && !strings.HasPrefix(url, "https:") {
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

func parseTopics(raw []byte) (*topics, error) {
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse collection: %w", err)
	}
	t := &topics{byID: map[string]*Item{}}
	for _, rawItem := range doc.Items {
		var d struct {
			ID         string                     `json:"id"`
			Properties map[string]json.RawMessage `json:"properties"`
			Assets     json.RawMessage            `json:"assets"`
		}
		if json.Unmarshal(rawItem, &d) != nil {
			continue
		}
		h := fnv.New64a()
		h.Write(rawItem)
		it := &Item{ID: d.ID, Version: strconv.FormatUint(h.Sum64(), 36)}
		assets, _ := ObjectFields(d.Assets)
		for _, a := range assets {
			var v struct {
				Href      string   `json:"href"`
				Tolerance *float64 `json:"ugs:simplify_tolerance"`
			}
			if json.Unmarshal(a.Value, &v) == nil {
				it.Assets = append(it.Assets, Asset{a.Key, v.Href, v.Tolerance})
			}
		}
		if it.Data() == "" {
			continue
		}
		if raw, ok := d.Properties["title"]; ok {
			_ = json.Unmarshal(raw, &it.Title)
		}
		if raw, ok := d.Properties["description"]; ok {
			_ = json.Unmarshal(raw, &it.Description)
		}
		renders, _ := ObjectFields(d.Properties["ugs:renders"])
		for _, r := range renders {
			var v struct {
				Title   *string `json:"title"`
				EsriURL string  `json:"esri_url"`
			}
			_ = json.Unmarshal(r.Value, &v)
			it.Renders = append(it.Renders, Render{r.Key, v.Title, v.EsriURL})
		}
		if len(it.Renders) == 0 {
			it.Renders = []Render{{Name: "default"}}
		}
		if _, dup := t.byID[it.ID]; !dup {
			t.order = append(t.order, it)
		} else {
			for i, o := range t.order {
				if o.ID == it.ID {
					t.order[i] = it
				}
			}
		}
		t.byID[it.ID] = it
	}
	return t, nil
}
