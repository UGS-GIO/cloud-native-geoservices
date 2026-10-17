// Package server is the HTTP side: routes, parameters, limits, Esri-style errors, response
// formats, and the encoded-response cache with its ETags.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/lru"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/provider"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/query"
)

// Limits protect a public endpoint.
type Limits struct {
	MaxWhere     int           // characters in `where`
	MaxBody      int64         // bytes of a POST body
	MaxObjectIDs int           // ids in `objectIds`
	Timeout      time.Duration // per request
	MaxScans     int           // full-layer scans (where/statistics/distinct without a bbox or page) at once
	ScanWait     time.Duration // how long a scan waits for a slot before a 503
	MaxWork      int           // queries that are not light (see light) computing at once
	WorkWait     time.Duration // how long one waits for a slot before a 503
	MaxLight     int           // light queries computing at once (cache hits are not counted)
	MaxInFlight  int           // requests of any kind at once; past it, an immediate 503
}

// Server serves the FeatureServer API.
type Server struct {
	Provider  *provider.Provider
	CacheTTL  int    // seconds, for Cache-Control
	PublicURL string // base for the URLs `/` lists; the request's host when empty
	Limits    Limits
	Log       *slog.Logger
	responses *lru.Cache[string, *encoded]
	scans     chan struct{}
	work      chan struct{}
	light     chan struct{}
	inFlight  chan struct{}
}

// New makes a server whose encoded-response cache holds responseBytes.
func New(p *provider.Provider, ttl int, publicURL string, limits Limits, responseBytes int64, log *slog.Logger) *Server {
	return &Server{Provider: p, CacheTTL: ttl, PublicURL: publicURL, Limits: limits, Log: log,
		responses: lru.New[string, *encoded](responseBytes), scans: make(chan struct{}, max(limits.MaxScans, 1)),
		work: make(chan struct{}, max(limits.MaxWork, 1)), light: make(chan struct{}, max(limits.MaxLight, 1)),
		inFlight: make(chan struct{}, max(limits.MaxInFlight, 1))}
}

// Handler routes requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.root)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		s.send(w, r, &encoded{body: jsonx.Marshal(jsonx.Obj{{K: "ok", V: true}}), contentType: jsonType}, "")
	})
	mux.HandleFunc("/ugs/rest/info", s.restInfo)
	mux.HandleFunc("/ugs/rest/generateToken", func(w http.ResponseWriter, r *http.Request) {
		s.send(w, r, &encoded{body: []byte("{}"), contentType: jsonType}, "")
	})
	mux.HandleFunc("/ugs/rest/services/{topic}/{render}/FeatureServer", s.service)
	mux.HandleFunc("/ugs/rest/services/{topic}/{render}/FeatureServer/{layer}", s.service)
	mux.HandleFunc("/ugs/rest/services/{topic}/{render}/FeatureServer/{layer}/{op}", s.service)
	mux.HandleFunc("/ugs/rest/services/{topic}/{render}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.PathValue("rest"), "MapServer") {
			s.fail(w, r, query.Bad("Invalid URL", "Invalid URL"))
			return
		}
		notFound(w, r)
	})
	mux.HandleFunc("/", notFound)
	return s.guard(mux)
}

func (s *Server) baseURL(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimSuffix(s.PublicURL, "/")
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	return proto + "://" + r.Host
}

// root lists every topic and render, with the URL to paste into Map Viewer's "Add layer from URL".
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	items, err := s.Provider.Catalog.Topics(r.Context())
	if err != nil {
		s.sendStatus(w, http.StatusBadGateway, jsonx.Obj{{K: "error", V: err.Error()}})
		return
	}
	base := s.baseURL(r)
	layers := []any{}
	for _, it := range items {
		for _, rd := range it.Renders {
			title := it.ID
			if rd.Title != nil {
				title = *rd.Title
			} else if it.Title != nil {
				title = *it.Title
			}
			layers = append(layers, jsonx.Obj{{K: "topic", V: it.ID}, {K: "render", V: rd.Name}, {K: "title", V: title},
				{K: "styled", V: rd.EsriURL != ""},
				{K: "url", V: base + "/ugs/rest/services/" + url.PathEscape(it.ID) + "/" + url.PathEscape(rd.Name) + "/FeatureServer/0"}})
		}
	}
	s.send(w, r, &encoded{body: jsonx.Marshal(jsonx.Obj{{K: "layers", V: layers}}), contentType: jsonType}, "")
}

func (s *Server) restInfo(w http.ResponseWriter, r *http.Request) {
	p, _, err := s.params(r)
	if err == nil {
		err = infoFormat(p)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Koop always advertises an https token URL on the request's host.
	s.send(w, r, encode(p, jsonx.Obj{{K: "currentVersion", V: 11.2}, {K: "fullVersion", V: "11.2.0"},
		{K: "authInfo", V: jsonx.Obj{{K: "isTokenBasedSecurity", V: true},
			{K: "tokenServicesUrl", V: "https://" + r.Host + "/ugs/rest/generateToken"}}}}), "")
}

func infoFormat(p query.Params) error {
	if v, ok := p["f"]; ok {
		if f, _ := v.(string); f != "json" && f != "pjson" {
			return query.Bad("Invalid format")
		}
	}
	return nil
}

var layerID = regexp.MustCompile(`^[^/]+$`)

// service handles the FeatureServer routes: service, layers and layer descriptions, and query.
func (s *Server) service(w http.ResponseWriter, r *http.Request) {
	topic, render := r.PathValue("topic"), r.PathValue("render")
	layer, op := r.PathValue("layer"), r.PathValue("op")
	if op != "" && op != "info" && op != "query" {
		notFound(w, r)
		return
	}
	isQuery := op == "query"
	p, raw, err := s.params(r)
	if err == nil && isQuery {
		err = s.checkLimits(raw)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	version, err := s.Provider.Version(r.Context(), topic, render)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tag := etag(version, r)
	if notModified(r, tag) {
		s.send(w, r, nil, tag)
		return
	}
	if hit, ok := s.responses.Get(tag); ok {
		s.send(w, r, hit, tag)
		return
	}
	// A full scan waits for its own few slots first, so scans queued behind each other hold no work slot.
	if isQuery && fullScan(raw) {
		release, err := slot(r.Context(), s.scans, s.Limits.ScanWait, errScanBusy)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer release()
	}
	// Every computed query holds a slot, so memory is bounded however many arrive at once.
	if isQuery {
		slots := s.light
		if !light(raw) {
			slots = s.work
		}
		release, err := slot(r.Context(), slots, s.Limits.WorkWait, errWorkBusy)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer release()
	}
	out, err := s.answer(r.Context(), topic, render, layer, isQuery, p, raw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out = pack(out)
	s.responses.Put(tag, out, out.size())
	s.send(w, r, out, tag)
}

// answer loads the topic and encodes the response the route asks for.
func (s *Server) answer(ctx context.Context, topic, render, layer string, isQuery bool, p query.Params, raw query.Raw) (*encoded, error) {
	c, err := s.Provider.Load(ctx, topic, render, isQuery, raw)
	if err != nil {
		return nil, err
	}
	if !isQuery {
		if err := infoFormat(p); err != nil {
			return nil, err
		}
		switch {
		case layer == "":
			return encode(p, esri.Server(c.Meta)), nil
		case layer == "layers":
			return encode(p, esri.Layers(c.Meta)), nil
		case layerID.MatchString(layer):
			return encode(p, esri.Layer(c.Meta, layer)), nil
		}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	res, err := query.Run(c, p)
	if err != nil {
		return nil, err
	}
	return encodeQuery(p, res)
}
