// conformance-record copies each reference layer of a conformance tier to GeoParquet and records
// the reference server's answers to the request set (README.md, Conformance).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UGS-GIO/geoparquet-featureserver/conformance"
)

// client is polite to public servers: one request at a time, a pause between requests, and
// retries with backoff on network errors, 429 and 5xx.
type client struct {
	http *http.Client
	gap  time.Duration
	last time.Time
}

const userAgent = "geoparquet-featureserver-conformance (+https://github.com/UGS-GIO/geoparquet-featureserver)"

func (c *client) do(ctx context.Context, method, u string, params url.Values) (int, string, []byte, time.Duration, error) {
	var err error
	for attempt := range 6 {
		if attempt > 0 {
			wait := time.Duration(1<<attempt) * time.Second
			log.Printf("retry %d in %s: %v", attempt, wait, err)
			time.Sleep(wait)
		}
		time.Sleep(time.Until(c.last.Add(c.gap)))
		c.last = time.Now()
		var req *http.Request
		if method == http.MethodPost {
			req, _ = http.NewRequestWithContext(ctx, method, u, strings.NewReader(params.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req, _ = http.NewRequestWithContext(ctx, method, u+"?"+params.Encode(), nil)
		}
		req.Header.Set("User-Agent", userAgent)
		start := time.Now()
		var res *http.Response
		res, err = c.http.Do(req)
		if err != nil {
			continue
		}
		body, rerr := io.ReadAll(res.Body)
		res.Body.Close()
		took := time.Since(start)
		if rerr != nil {
			err = rerr
			continue
		}
		if res.StatusCode == 429 || res.StatusCode >= 500 {
			err = fmt.Errorf("%s: %s", res.Status, body[:min(len(body), 200)])
			continue
		}
		return res.StatusCode, res.Header.Get("Content-Type"), body, took, nil
	}
	return 0, "", nil, 0, err
}

// json gets a JSON document, retrying Esri's in-body 5xx errors too.
func (c *client) json(ctx context.Context, u string, params url.Values, v any) error {
	for attempt := range 4 {
		_, _, body, _, err := c.do(ctx, http.MethodGet, u, params)
		if err != nil {
			return err
		}
		var e struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(body, &e)
		if e.Error == nil {
			d := json.NewDecoder(bytes.NewReader(body))
			d.UseNumber()
			return d.Decode(v)
		}
		if e.Error.Code < 500 || attempt == 3 {
			return fmt.Errorf("%s: %d %s", u, e.Error.Code, e.Error.Message)
		}
		time.Sleep(time.Duration(2<<attempt) * time.Second)
	}
	return nil
}

func main() {
	tier := flag.String("tier", "small", "small or large")
	layersPath := flag.String("layers", "testdata/conformance/layers.json", "reference layers")
	requestsPath := flag.String("requests", "testdata/conformance/requests.json", "request templates")
	out := flag.String("out", "", "output directory (small: testdata/conformance/layers)")
	only := flag.String("only", "", "comma-separated layer names")
	skipExtract := flag.Bool("skip-extract", false, "re-record responses, keep the data files")
	add := flag.String("names", "", "comma-separated request names to record into the existing recordings (implies -skip-extract)")
	gap := flag.Duration("gap", 250*time.Millisecond, "pause between requests")
	flag.Parse()

	var layers conformance.Layers
	var requests []conformance.Request
	if err := conformance.ReadJSON(*layersPath, &layers); err != nil {
		log.Fatal(err)
	}
	if err := conformance.ReadJSON(*requestsPath, &requests); err != nil {
		log.Fatal(err)
	}
	list := layers.Small
	if *tier == "large" {
		list = layers.Large
	}
	if *out == "" {
		if *tier != "small" {
			log.Fatal("-out is required for the large tier")
		}
		*out = "testdata/conformance/layers"
	}
	c := &client{http: &http.Client{Timeout: 5 * time.Minute}, gap: *gap}
	ctx := context.Background()
	for _, l := range list {
		if *only != "" && !slices.Contains(strings.Split(*only, ","), l.Name) {
			continue
		}
		dir := filepath.Join(*out, l.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		meta, err := layerMeta(ctx, c, l.URL)
		if err != nil {
			log.Fatal(err)
		}
		if !*skipExtract && *add == "" {
			start := time.Now()
			n, err := extract(ctx, c, l.URL, meta, conformance.DataPath(*out, l.Name))
			if err != nil {
				log.Fatalf("%s: %v", l.Name, err)
			}
			log.Printf("%s: %d features in %s", l.Name, n, time.Since(start).Round(time.Second))
		}
		if err := record(ctx, c, l, meta, requests, *add, conformance.RecordingPath(*out, l.Name)); err != nil {
			log.Fatalf("%s: %v", l.Name, err)
		}
	}
}

// meta is what the recorder reads from a layer description.
type meta struct {
	ObjectIDField  string  `json:"objectIdField"`
	MaxRecordCount int     `json:"maxRecordCount"`
	GeometryType   string  `json:"geometryType"`
	Fields         []field `json:"fields"`
}

type field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func layerMeta(ctx context.Context, c *client, u string) (*meta, error) {
	var m meta
	if err := c.json(ctx, u, url.Values{"f": {"json"}}, &m); err != nil {
		return nil, err
	}
	if m.ObjectIDField == "" || m.MaxRecordCount == 0 {
		return nil, fmt.Errorf("%s: no objectIdField or maxRecordCount", u)
	}
	return &m, nil
}

// derived adds the vars the recorder works out: an offset near the end, an envelope in Web
// Mercator and as JSON, the point as JSON, and Map Viewer's quantization parameters for a tile.
func derived(ctx context.Context, c *client, l conformance.Layer, m *meta) (map[string]string, error) {
	vars := map[string]string{}
	for k, v := range l.Vars {
		vars[k] = v
	}
	var count struct {
		Count int `json:"count"`
	}
	err := c.json(ctx, l.URL+"/query", url.Values{"where": {"1=1"}, "returnCountOnly": {"true"}, "f": {"json"}}, &count)
	if err != nil {
		return nil, err
	}
	vars["lastoffset"] = strconv.Itoa(max(count.Count-5, 0))
	vars["maxrecords"] = strconv.Itoa(m.MaxRecordCount)
	if env, ok := l.Vars["env"]; ok {
		b, err := box(env)
		if err != nil {
			return nil, err
		}
		x0, y0 := mercator(b[0], b[1])
		x1, y1 := mercator(b[2], b[3])
		vars["env3857"] = fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", x0, y0, x1, y1)
		vars["envext"] = fmt.Sprintf(`{"xmin":%v,"ymin":%v,"xmax":%v,"ymax":%v,"spatialReference":{"wkid":4326}}`, b[0], b[1], b[2], b[3])
	}
	if pt, ok := l.Vars["pt"]; ok {
		x, y, _ := strings.Cut(pt, ",")
		vars["ptjson"] = fmt.Sprintf(`{"x":%s,"y":%s}`, x, y)
	}
	if tile, ok := l.Vars["tile"]; ok {
		b, err := box(tile)
		if err != nil {
			return nil, err
		}
		vars["tilequant"] = fmt.Sprintf(`{"extent":{"spatialReference":{"wkid":102100},"xmin":%v,"ymin":%v,"xmax":%v,"ymax":%v},"mode":"view","originPosition":"upperLeft","tolerance":%v}`,
			b[0], b[1], b[2], b[3], (b[2]-b[0])/512)
		vars["tilegeom"] = fmt.Sprintf(`{"xmin":%v,"ymin":%v,"xmax":%v,"ymax":%v,"spatialReference":{"wkid":102100}}`, b[0], b[1], b[2], b[3])
	}
	return vars, nil
}

func box(s string) ([4]float64, error) {
	var b [4]float64
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return b, fmt.Errorf("envelope %q: want xmin,ymin,xmax,ymax", s)
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return b, err
		}
		b[i] = v
	}
	return b, nil
}

func mercator(lon, lat float64) (float64, float64) {
	const r = 6378137.0
	return r * lon * math.Pi / 180, r * math.Log(math.Tan(math.Pi/4+lat*math.Pi/360))
}

// record asks the reference layer each request and writes the answers. With names set it asks
// only those and keeps the other answers already recorded at path.
func record(ctx context.Context, c *client, l conformance.Layer, m *meta, requests []conformance.Request, names, path string) error {
	vars, err := derived(ctx, c, l, m)
	if err != nil {
		return err
	}
	rec := conformance.Recording{Layer: l, Recorded: time.Now().UTC().Format(time.RFC3339)}
	var old conformance.Recording
	if names != "" {
		if err := conformance.ReadJSON(path, &old); err != nil {
			return err
		}
		rec.Recorded = old.Recorded
	}
	for _, tmpl := range requests {
		if names != "" && !slices.Contains(strings.Split(names, ","), tmpl.Name) {
			if i := slices.IndexFunc(old.Responses, func(r conformance.Response) bool { return r.Name == tmpl.Name }); i >= 0 {
				rec.Responses = append(rec.Responses, old.Responses[i])
			}
			continue
		}
		req, ok := conformance.Fill(tmpl, vars)
		if !ok {
			continue
		}
		params := url.Values{}
		for k, v := range conformance.Resolve(req.Params, m.ObjectIDField) {
			params.Set(k, v)
		}
		u := l.URL
		if req.Op != "layer" {
			u += "/query"
		}
		method := req.Method
		if method == "" {
			method = http.MethodGet
		}
		status, ct, body, took, err := c.do(ctx, method, u, params)
		if err != nil {
			return fmt.Errorf("%s: %w", req.Name, err)
		}
		r := conformance.Response{Request: req, Status: status, ContentType: ct, Millis: took.Milliseconds()}
		if mt, _, _ := mime.ParseMediaType(ct); strings.Contains(mt, "json") || json.Valid(body) {
			var buf bytes.Buffer
			if err := json.Compact(&buf, body); err != nil {
				return fmt.Errorf("%s: %w", req.Name, err)
			}
			r.Body = buf.Bytes()
		} else {
			r.PBF = body
		}
		log.Printf("%s/%s: %d %s %d bytes %s", l.Name, req.Name, status, ct, len(body), took.Round(time.Millisecond))
		rec.Responses = append(rec.Responses, r)
	}
	return write(path, rec)
}

// write puts each response on a line of its own, so a re-recording diffs by request.
func write(path string, rec conformance.Recording) error {
	head, err := json.Marshal(struct {
		Layer    conformance.Layer `json:"layer"`
		Recorded string            `json:"recorded"`
	}{rec.Layer, rec.Recorded})
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.Write(head[:len(head)-1])
	b.WriteString(",\"responses\":[\n")
	for i, r := range rec.Responses {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteString(",\n")
		}
		b.Write(line)
	}
	b.WriteString("\n]}\n")
	return os.WriteFile(path, b.Bytes(), 0o644)
}
