// geoservices: an ArcGIS FeatureServer over the warehouse GeoParquet on the CDN (README.md).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/netutil"
	"golang.org/x/sync/semaphore"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/catalog"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/provider"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/server"
)

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	n, err := strconv.ParseInt(env(key, ""), 10, 64)
	if err != nil {
		return def
	}
	return n
}

// memoryLimit is the smallest memory.max on this process's cgroup v2 path (in a container that is
// the container's limit), or 0 when unbounded.
func memoryLimit() int64 {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	var path string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			path = p
		}
	}
	var limit int64
	for dir := "/sys/fs/cgroup" + path; strings.HasPrefix(dir, "/sys/fs/cgroup"); dir = filepath.Dir(dir) {
		v, err := os.ReadFile(filepath.Join(dir, "memory.max"))
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64); err == nil && n > 0 && (limit == 0 || n < limit) {
			limit = n
		}
	}
	return limit
}

// budget is env key in MB, else the smaller of def MB and a fraction of the memory limit.
func budget(key string, defMB int64, fraction float64, limit int64) int64 {
	if v := envInt(key, -1); v >= 0 {
		return v << 20
	}
	b := defMB << 20
	if limit > 0 {
		b = min(b, int64(float64(limit)*fraction))
	}
	return b
}

func main() {
	level := slog.LevelWarn
	switch strings.ToLower(env("LOG_LEVEL", "warn")) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	// Size everything from the container: a soft heap limit at 80%, caches as fractions of it.
	limit := memoryLimit()
	if limit > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(limit / 10 * 8)
		if os.Getenv("GOGC") == "" && limit >= 2<<30 {
			debug.SetGCPercent(200) // with room and a memory limit to stop it, GC less often
		}
	}
	ttl := envInt("CACHE_TTL", 300)
	// Requests and open connections at once: 2048, or one per MiB of memory when that is less.
	maxInFlight := envInt("MAX_IN_FLIGHT", 2048)
	if limit > 0 && os.Getenv("MAX_IN_FLIGHT") == "" {
		maxInFlight = min(maxInFlight, limit>>20)
	}
	geometryBudget := budget("GEOMETRY_BUDGET_MB", 512, 0.125, limit)
	cat := catalog.New(env("STAC_COLLECTION_URL",
		"https://maps-assets.geology.utah.gov/warehouse/stac/ugs-serving-topics/items.json"), time.Duration(ttl)*time.Second)
	prov := &provider.Provider{
		Catalog: cat,
		Reader: pq.NewReader(budget("PAGE_CACHE_MB", 128, 0.03, limit),
			budget("DECODED_CACHE_MB", 512, 0.125, limit), geometryBudget),
		Shapes:          geom.NewShapes(budget("SHAPE_CACHE_MB", 256, 0.0625, limit)),
		MaxRecords:      envInt("MAX_RECORDS", 2000),
		TileMaxRecords:  envInt("TILE_MAX_RECORDS", 8000),
		MaxGeometryRows: envInt("MAX_GEOMETRY_ROWS", 50000),
		GeometryBudget:  semaphore.NewWeighted(geometryBudget),
		BudgetBytes:     geometryBudget,
		BudgetWait:      time.Duration(envInt("BUDGET_WAIT_MS", 5000)) * time.Millisecond,
		Mirror:          env("DATA_MIRROR", ""),
		HeavyBytes:      envInt("HEAVY_MB", 8) << 20,
		HeavySlots:      make(chan struct{}, max(1, envInt("HEAVY_SLOTS", int64(runtime.GOMAXPROCS(0)-1)))),
	}
	srv := server.New(prov, int(ttl), env("PUBLIC_URL", ""), server.Limits{
		MaxWhere:     int(envInt("MAX_WHERE_LENGTH", 4096)),
		MaxBody:      envInt("MAX_BODY_BYTES", 1<<20),
		MaxObjectIDs: int(envInt("MAX_OBJECT_IDS", 10000)),
		Timeout:      time.Duration(envInt("REQUEST_TIMEOUT_S", 30)) * time.Second,
		MaxScans:     int(envInt("MAX_SCANS", 2)),
		ScanWait:     time.Duration(envInt("SCAN_WAIT_MS", 2000)) * time.Millisecond,
		MaxWork:      int(envInt("MAX_WORK", int64(4*runtime.GOMAXPROCS(0)))),
		WorkWait:     time.Duration(envInt("WORK_WAIT_MS", 2000)) * time.Millisecond,
		MaxLight:     int(envInt("MAX_LIGHT", int64(16*runtime.GOMAXPROCS(0)))),
		MaxInFlight:  int(maxInFlight),
	}, budget("RESPONSE_CACHE_MB", 256, 0.0625, limit), log)

	if addr := env("PPROF_ADDR", ""); addr != "" {
		// Profiling, off by default: PPROF_ADDR=localhost:6060.
		go func() { log.Warn("pprof", "err", http.ListenAndServe(addr, nil)) }()
	}
	hs := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}

	// Cloud Run sends SIGTERM before stopping an instance: finish what is in flight.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Warn("geoservices listening", "addr", hs.Addr, "memory_limit", limit)
	ln, err := net.Listen("tcp", hs.Addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	// Past the cap, connections wait in the kernel's backlog instead of in our memory.
	if err := hs.Serve(netutil.LimitListener(ln, int(maxInFlight))); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
