// Package ocicachegc performs mark-and-sweep garbage collection of the
// shared OCI cache at data_dir/system/oci-cache.
//
// The cache stores content-addressed blobs under blobs/sha256/ and an
// index.json that references one or more manifests. Blobs are written
// whenever an image is pulled or pushed and are never removed by the
// normal code paths, so without a GC the cache grows unbounded.
//
// The collector walks index.json and every manifest (or manifest index)
// reachable from it to build the set of "live" blob digests, then
// deletes any blob that is not live. Blobs whose mtime is within
// MinBlobAge are always kept; this is the grace period used to avoid
// racing with concurrent pulls, which write layer blobs before updating
// index.json.
package ocicachegc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// RootsProvider returns extra manifest digests (in "sha256:<hex>" form)
// that should be treated as live alongside everything reachable from
// index.json. Used for blobs the registry tracks in memory but does not
// root in the OCI layout (e.g. BuildKit cache exports under cache/*).
type RootsProvider interface {
	LiveCacheManifestDigests() []string
}

// Collector garbage-collects the shared OCI cache.
type Collector struct {
	paths      *paths.Paths
	interval   time.Duration
	minBlobAge time.Duration
	logger     *slog.Logger
	metrics    *Metrics
	tracer     trace.Tracer
	roots      RootsProvider
	now        func() time.Time
	mu         sync.Mutex
}

// Stats summarises the outcome of one Collect pass.
type Stats struct {
	LiveBlobs     int
	ScannedBlobs  int
	DeletedBlobs  int
	DeletedBytes  int64
	SkippedRecent int
}

// NewCollector creates a collector. minBlobAge is the minimum age a blob
// must have before it becomes eligible for deletion; it protects blobs
// that are currently being written by a concurrent pull or push. roots
// may be nil; when non-nil it is consulted on every sweep for additional
// manifest digests to mark live.
func NewCollector(p *paths.Paths, interval, minBlobAge time.Duration, roots RootsProvider, logger *slog.Logger, meter metric.Meter, tracer trace.Tracer) (*Collector, error) {
	if p == nil {
		return nil, errors.New("paths is required")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %s", interval)
	}
	if minBlobAge < 0 {
		return nil, fmt.Errorf("min_blob_age cannot be negative, got %s", minBlobAge)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if tracer == nil {
		tracer = otel.Tracer("hypeman/oci_cache_gc")
	}
	c := &Collector{
		paths:      p,
		interval:   interval,
		minBlobAge: minBlobAge,
		logger:     logger.With("component", "oci_cache_gc"),
		tracer:     tracer,
		roots:      roots,
		now:        time.Now,
	}
	if meter != nil {
		m, err := newMetrics(meter)
		if err != nil {
			return nil, fmt.Errorf("create oci cache gc metrics: %w", err)
		}
		c.metrics = m
	}
	return c, nil
}

// Run performs one Collect pass immediately and then every interval until
// ctx is cancelled. It never returns an error; individual sweep failures
// are logged and metrics are recorded, but the loop keeps running.
func (c *Collector) Run(ctx context.Context) error {
	c.logger.InfoContext(ctx, "oci cache gc started", "interval", c.interval, "min_blob_age", c.minBlobAge)
	if _, err := c.Collect(ctx); err != nil {
		c.logger.ErrorContext(ctx, "oci cache gc sweep failed", "error", err)
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := c.Collect(ctx); err != nil {
				c.logger.ErrorContext(ctx, "oci cache gc sweep failed", "error", err)
			}
		}
	}
}

// Collect performs one full mark-and-sweep pass over the OCI cache.
func (c *Collector) Collect(ctx context.Context) (Stats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, span := c.tracer.Start(ctx, "oci_cache_gc.sweep")
	defer span.End()

	start := c.now()
	stats, err := c.collect(ctx)
	status := "success"
	if err != nil {
		status = "error"
	}
	if c.metrics != nil {
		c.metrics.RecordSweep(ctx, status, c.now().Sub(start), stats)
	}
	span.SetAttributes(
		attribute.Int("live_blobs", stats.LiveBlobs),
		attribute.Int("scanned_blobs", stats.ScannedBlobs),
		attribute.Int("deleted_blobs", stats.DeletedBlobs),
		attribute.Int64("deleted_bytes", stats.DeletedBytes),
		attribute.Int("skipped_recent", stats.SkippedRecent),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return stats, err
	}
	span.SetStatus(codes.Ok, "")
	logFn := c.logger.DebugContext
	if stats.DeletedBlobs > 0 {
		logFn = c.logger.InfoContext
	}
	logFn(ctx, "oci cache gc sweep completed",
		"live_blobs", stats.LiveBlobs,
		"scanned_blobs", stats.ScannedBlobs,
		"deleted_blobs", stats.DeletedBlobs,
		"deleted_bytes", stats.DeletedBytes,
		"skipped_recent", stats.SkippedRecent,
	)
	return stats, nil
}

func (c *Collector) collect(ctx context.Context) (Stats, error) {
	var stats Stats

	blobDir := c.paths.OCICacheBlobDir()
	if _, err := os.Stat(blobDir); errors.Is(err, fs.ErrNotExist) {
		return stats, nil
	} else if err != nil {
		return stats, fmt.Errorf("stat blob dir: %w", err)
	}

	var extraRoots []string
	if c.roots != nil {
		extraRoots = c.roots.LiveCacheManifestDigests()
	}
	_, markSpan := c.tracer.Start(ctx, "oci_cache_gc.mark")
	live, err := liveBlobs(c.paths, extraRoots)
	if err != nil {
		markSpan.RecordError(err)
		markSpan.SetStatus(codes.Error, err.Error())
		markSpan.End()
		return stats, fmt.Errorf("compute live set: %w", err)
	}
	stats.LiveBlobs = len(live)
	markSpan.SetAttributes(
		attribute.Int("live_blobs", stats.LiveBlobs),
		attribute.Int("extra_roots", len(extraRoots)),
	)
	markSpan.End()

	cutoff := c.now().Add(-c.minBlobAge)

	entries, err := os.ReadDir(blobDir)
	if err != nil {
		return stats, fmt.Errorf("read blob dir: %w", err)
	}

	ctx, sweepSpan := c.tracer.Start(ctx, "oci_cache_gc.sweep_blobs",
		trace.WithAttributes(attribute.Int("blob_dir_entries", len(entries))),
	)
	defer func() {
		sweepSpan.SetAttributes(
			attribute.Int("scanned_blobs", stats.ScannedBlobs),
			attribute.Int("deleted_blobs", stats.DeletedBlobs),
			attribute.Int("skipped_recent", stats.SkippedRecent),
		)
		sweepSpan.End()
	}()

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isBlobName(name) {
			continue
		}
		stats.ScannedBlobs++
		if _, ok := live[name]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return stats, fmt.Errorf("stat blob %s: %w", name, err)
		}
		if info.ModTime().After(cutoff) {
			stats.SkippedRecent++
			continue
		}
		path := filepath.Join(blobDir, name)
		size := info.Size()
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return stats, fmt.Errorf("remove blob %s: %w", name, err)
		}
		stats.DeletedBlobs++
		stats.DeletedBytes += size
		c.logger.DebugContext(ctx, "deleted unreferenced oci blob", "digest", name, "size", size)
	}

	return stats, nil
}

// blobNamePattern matches a valid sha256 blob filename (64 hex chars).
// Temporary files (e.g. "<digest>.tmp" written by the blob store) are
// intentionally excluded.
var blobNamePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isBlobName(name string) bool {
	return blobNamePattern.MatchString(name)
}

// descriptor captures the subset of OCI/Docker descriptor fields we care
// about when walking manifests. Fields we do not use (urls, platform,
// size, annotations, mediaType) are omitted.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

// manifestDoc is a union of the fields found on OCI image manifests,
// OCI image indexes, Docker v2 manifests, and Docker v2 manifest lists.
// Decoding into one shape lets us traverse either format without
// branching on the mediaType, which is often unreliable in practice.
type manifestDoc struct {
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"`
	Subject   *descriptor  `json:"subject,omitempty"`
}

// liveBlobs returns the set of blob hex digests reachable from the OCI
// cache index.json plus any extraRoots provided. Keys are bare hex
// (no "sha256:" prefix), matching the filenames under blobs/sha256/.
// extraRoots entries are accepted in either "sha256:<hex>" or bare hex
// form and silently skipped if malformed.
func liveBlobs(p *paths.Paths, extraRoots []string) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	visited := make(map[string]struct{})

	indexPath := p.OCICacheIndex()
	data, err := os.ReadFile(indexPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Cache hasn't been initialised yet; only extraRoots are live.
	case err != nil:
		return nil, fmt.Errorf("read index.json: %w", err)
	default:
		var index manifestDoc
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("parse index.json: %w", err)
		}
		for _, m := range index.Manifests {
			if err := walkDescriptor(p, m, live, visited); err != nil {
				return nil, err
			}
		}
		// OCI v1.1 allows index.json itself to carry a subject descriptor;
		// recurse into it so anything reachable that way stays marked.
		if index.Subject != nil {
			if err := walkDescriptor(p, *index.Subject, live, visited); err != nil {
				return nil, err
			}
		}
	}

	for _, digest := range extraRoots {
		if err := walkDescriptor(p, descriptor{Digest: normaliseDigest(digest)}, live, visited); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// walkDescriptor records desc.Digest in live, then if the blob is a
// manifest (or manifest index) recurses into its referenced blobs.
// Unparseable blobs are treated as opaque leaves (recorded but not
// descended into) so the sweep errs on the side of keeping data when
// disk contents are malformed.
func walkDescriptor(p *paths.Paths, desc descriptor, live, visited map[string]struct{}) error {
	hex, ok := digestHex(desc.Digest)
	if !ok {
		return nil
	}
	if _, seen := visited[hex]; seen {
		return nil
	}
	visited[hex] = struct{}{}
	live[hex] = struct{}{}

	blobPath := p.OCICacheBlob(hex)
	data, err := os.ReadFile(blobPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read blob %s: %w", hex, err)
	}

	var doc manifestDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	if h, ok := digestHex(doc.Config.Digest); ok {
		live[h] = struct{}{}
	}
	for _, layer := range doc.Layers {
		if h, ok := digestHex(layer.Digest); ok {
			live[h] = struct{}{}
		}
	}
	if doc.Subject != nil {
		if err := walkDescriptor(p, *doc.Subject, live, visited); err != nil {
			return err
		}
	}
	for _, m := range doc.Manifests {
		if err := walkDescriptor(p, m, live, visited); err != nil {
			return err
		}
	}
	return nil
}

// normaliseDigest accepts either "sha256:<hex>" or a bare 64-char hex
// string and returns the "sha256:<hex>" form expected by walkDescriptor.
// Anything else is returned unchanged so digestHex can reject it.
func normaliseDigest(d string) string {
	if len(d) == 64 && blobNamePattern.MatchString(d) {
		return "sha256:" + d
	}
	return d
}

// digestHex extracts the hex portion of a "sha256:<hex>" digest. It
// returns false for empty strings, unsupported algorithms, or malformed
// hex so callers can skip bad data without erroring the sweep.
func digestHex(d string) (string, bool) {
	if len(d) != len("sha256:")+64 {
		return "", false
	}
	if d[:7] != "sha256:" {
		return "", false
	}
	hex := d[7:]
	if !blobNamePattern.MatchString(hex) {
		return "", false
	}
	return hex, true
}
