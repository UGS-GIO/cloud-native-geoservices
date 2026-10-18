package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Get reads a URL or local file.
type Get func(ctx context.Context, url string) ([]byte, error)

// Source lists the topics to serve. Geometry type, extent and fields come from each file's
// GeoParquet metadata, not from the source.
type Source interface {
	Items(ctx context.Context, get Get) ([]*Item, error)
}

// STAC lists the items of a STAC ItemCollection (an items.json) that have a `data` asset. It also
// reads `ugs:renders` and the `ugs:simplify_tolerance` of overview assets.
type STAC struct{ URL string }

// Folder serves each .parquet file in Dir, with its file name, less the extension, as the id.
type Folder struct{ Dir string }

// Config lists topics in a JSON file (README.md). Relative URLs resolve against the file's own.
type Config struct{ URL string }

func (s STAC) Items(ctx context.Context, get Get) ([]*Item, error) {
	raw, err := get(ctx, s.URL)
	if err != nil {
		return nil, err
	}
	return parseSTAC(raw)
}

func (f Folder) Items(context.Context, Get) ([]*Item, error) {
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return nil, err
	}
	var items []*Item
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".parquet")
		if !ok || e.IsDir() {
			continue
		}
		path := filepath.Join(f.Dir, e.Name())
		items = append(items, &Item{ID: id, Assets: []Asset{{Key: "data", Href: path}}, Version: hash([]byte(path))})
	}
	return items, nil
}

func (c Config) Items(ctx context.Context, get Get) ([]*Item, error) {
	raw, err := get(ctx, c.URL)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Layers []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.URL, err)
	}
	items := make([]*Item, 0, len(doc.Layers))
	for i, rawLayer := range doc.Layers {
		var l struct {
			ID          string   `json:"id"`
			Title       *string  `json:"title"`
			Description string   `json:"description"`
			URL         string   `json:"url"`
			Renders     []Render `json:"renders"`
		}
		if err := json.Unmarshal(rawLayer, &l); err != nil {
			return nil, fmt.Errorf("parse %s: layer %d: %w", c.URL, i, err)
		}
		if l.ID == "" || l.URL == "" {
			return nil, fmt.Errorf("%s: layer %d needs an id and a url", c.URL, i)
		}
		for j := range l.Renders {
			if l.Renders[j].EsriURL != "" {
				l.Renders[j].EsriURL = resolve(c.URL, l.Renders[j].EsriURL)
			}
		}
		items = append(items, &Item{ID: l.ID, Title: l.Title, Description: l.Description, Renders: l.Renders,
			Assets: []Asset{{Key: "data", Href: resolve(c.URL, l.URL)}}, Version: hash(rawLayer)})
	}
	return items, nil
}

func parseSTAC(raw []byte) ([]*Item, error) {
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse collection: %w", err)
	}
	var items []*Item
	for _, rawItem := range doc.Items {
		var d struct {
			ID         string                     `json:"id"`
			Properties map[string]json.RawMessage `json:"properties"`
			Assets     json.RawMessage            `json:"assets"`
		}
		if json.Unmarshal(rawItem, &d) != nil {
			continue
		}
		it := &Item{ID: d.ID, Version: hash(rawItem)}
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
		if raw := d.Properties["title"]; raw != nil {
			_ = json.Unmarshal(raw, &it.Title)
		}
		if raw := d.Properties["description"]; raw != nil {
			_ = json.Unmarshal(raw, &it.Description)
		}
		renders, _ := ObjectFields(d.Properties["ugs:renders"])
		for _, r := range renders {
			var v Render
			_ = json.Unmarshal(r.Value, &v)
			v.Name = r.Key
			it.Renders = append(it.Renders, v)
		}
		items = append(items, it)
	}
	return items, nil
}

func hash(b []byte) string {
	h := fnv.New64a()
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}

func isHTTP(u string) bool { return strings.HasPrefix(u, "http:") || strings.HasPrefix(u, "https:") }

// resolve reads ref relative to base, a URL or a local file.
func resolve(base, ref string) string {
	if isHTTP(base) {
		if b, err := url.Parse(base); err == nil {
			if r, err := b.Parse(ref); err == nil {
				return r.String()
			}
		}
		return ref
	}
	if isHTTP(ref) || filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(filepath.Dir(base), ref)
}
