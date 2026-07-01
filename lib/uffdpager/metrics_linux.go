//go:build linux

package uffdpager

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RegisterMetrics creates observable pager instruments whose callbacks read
// statsFn on each collection cycle. Returns nil when the meter or statsFn is
// nil so callers can wire this in unconditionally.
func RegisterMetrics(meter metric.Meter, versionKey string, statsFn func() Stats) error {
	if meter == nil || statsFn == nil {
		return nil
	}

	cacheHits, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_hits_total",
		metric.WithDescription("UFFD page cache hits"),
	)
	if err != nil {
		return fmt.Errorf("create cache hits counter: %w", err)
	}
	cacheMisses, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_misses_total",
		metric.WithDescription("UFFD page cache misses"),
	)
	if err != nil {
		return fmt.Errorf("create cache misses counter: %w", err)
	}
	faults, err := meter.Int64ObservableCounter(
		"hypeman_uffd_faults_total",
		metric.WithDescription("UFFD page faults handled"),
	)
	if err != nil {
		return fmt.Errorf("create faults counter: %w", err)
	}
	backingBytesRead, err := meter.Int64ObservableCounter(
		"hypeman_uffd_backing_bytes_read_total",
		metric.WithDescription("Bytes read from the backing memory file"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create backing bytes read counter: %w", err)
	}
	copies, err := meter.Int64ObservableCounter(
		"hypeman_uffd_copies_total",
		metric.WithDescription("UFFDIO_COPY operations issued"),
	)
	if err != nil {
		return fmt.Errorf("create copies counter: %w", err)
	}
	copyErrors, err := meter.Int64ObservableCounter(
		"hypeman_uffd_copy_errors_total",
		metric.WithDescription("UFFDIO_COPY operations that returned an error"),
	)
	if err != nil {
		return fmt.Errorf("create copy errors counter: %w", err)
	}
	cacheLookupNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_lookup_nanos_total",
		metric.WithDescription("Nanoseconds spent in cache lookups"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache lookup nanos counter: %w", err)
	}
	cacheAddNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_add_nanos_total",
		metric.WithDescription("Nanoseconds spent inserting cache entries"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache add nanos counter: %w", err)
	}
	faultNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_fault_nanos_total",
		metric.WithDescription("Nanoseconds spent handling faults end-to-end"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create fault nanos counter: %w", err)
	}
	readPageNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_read_page_nanos_total",
		metric.WithDescription("Nanoseconds spent reading pages"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create read page nanos counter: %w", err)
	}
	backingReadNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_backing_read_nanos_total",
		metric.WithDescription("Nanoseconds spent reading from the backing file"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create backing read nanos counter: %w", err)
	}
	copyNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_copy_nanos_total",
		metric.WithDescription("Nanoseconds spent in UFFDIO_COPY calls"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create copy nanos counter: %w", err)
	}

	cacheBytes, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_bytes",
		metric.WithDescription("Current UFFD page cache size"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create cache bytes gauge: %w", err)
	}
	cacheMaxBytes, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_max_bytes",
		metric.WithDescription("Configured UFFD page cache capacity"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create cache max bytes gauge: %w", err)
	}
	cacheItems, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_items",
		metric.WithDescription("Pages currently held in the UFFD cache"),
	)
	if err != nil {
		return fmt.Errorf("create cache items gauge: %w", err)
	}
	cacheShards, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_shards",
		metric.WithDescription("UFFD page cache shard count"),
	)
	if err != nil {
		return fmt.Errorf("create cache shards gauge: %w", err)
	}
	activeSessions, err := meter.Int64ObservableGauge(
		"hypeman_uffd_active_sessions",
		metric.WithDescription("Active pager sessions"),
	)
	if err != nil {
		return fmt.Errorf("create active sessions gauge: %w", err)
	}
	activeFaults, err := meter.Int64ObservableGauge(
		"hypeman_uffd_active_faults",
		metric.WithDescription("Faults currently in flight"),
	)
	if err != nil {
		return fmt.Errorf("create active faults gauge: %w", err)
	}
	maxConcurrentFaults, err := meter.Int64ObservableGauge(
		"hypeman_uffd_max_concurrent_faults",
		metric.WithDescription("High-water mark of concurrent in-flight faults"),
	)
	if err != nil {
		return fmt.Errorf("create max concurrent faults gauge: %w", err)
	}
	draining, err := meter.Int64ObservableGauge(
		"hypeman_uffd_draining",
		metric.WithDescription("1 if the pager is draining, 0 otherwise"),
	)
	if err != nil {
		return fmt.Errorf("create draining gauge: %w", err)
	}
	cacheLookupMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_lookup_max_nanos",
		metric.WithDescription("Max cache lookup latency"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache lookup max nanos gauge: %w", err)
	}
	cacheAddMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_add_max_nanos",
		metric.WithDescription("Max cache add latency"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache add max nanos gauge: %w", err)
	}
	faultMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_fault_max_nanos",
		metric.WithDescription("Max fault handling latency"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create fault max nanos gauge: %w", err)
	}
	readPageMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_read_page_max_nanos",
		metric.WithDescription("Max page read latency"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create read page max nanos gauge: %w", err)
	}
	backingReadMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_backing_read_max_nanos",
		metric.WithDescription("Max backing file read latency"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create backing read max nanos gauge: %w", err)
	}
	copyMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_copy_max_nanos",
		metric.WithDescription("Max UFFDIO_COPY latency"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create copy max nanos gauge: %w", err)
	}

	attrs := metric.WithAttributes(attribute.String("version_key", versionKey))

	_, err = meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			s := statsFn()
			o.ObserveInt64(cacheHits, s.CacheHits, attrs)
			o.ObserveInt64(cacheMisses, s.CacheMisses, attrs)
			o.ObserveInt64(faults, s.Faults, attrs)
			o.ObserveInt64(backingBytesRead, s.BackingBytesRead, attrs)
			o.ObserveInt64(copies, s.Copies, attrs)
			o.ObserveInt64(copyErrors, s.CopyErrors, attrs)
			o.ObserveInt64(cacheLookupNanos, s.CacheLookupNanos, attrs)
			o.ObserveInt64(cacheAddNanos, s.CacheAddNanos, attrs)
			o.ObserveInt64(faultNanos, s.FaultNanos, attrs)
			o.ObserveInt64(readPageNanos, s.ReadPageNanos, attrs)
			o.ObserveInt64(backingReadNanos, s.BackingReadNanos, attrs)
			o.ObserveInt64(copyNanos, s.CopyNanos, attrs)

			o.ObserveInt64(cacheBytes, s.CacheBytes, attrs)
			o.ObserveInt64(cacheMaxBytes, s.CacheMax, attrs)
			o.ObserveInt64(cacheItems, int64(s.CacheItems), attrs)
			o.ObserveInt64(cacheShards, int64(s.CacheShards), attrs)
			o.ObserveInt64(activeSessions, int64(s.ActiveSessions), attrs)
			o.ObserveInt64(activeFaults, s.ActiveFaults, attrs)
			o.ObserveInt64(maxConcurrentFaults, s.MaxConcurrentFaults, attrs)
			o.ObserveInt64(draining, boolToInt64(s.Draining), attrs)
			o.ObserveInt64(cacheLookupMaxNanos, s.CacheLookupMaxNanos, attrs)
			o.ObserveInt64(cacheAddMaxNanos, s.CacheAddMaxNanos, attrs)
			o.ObserveInt64(faultMaxNanos, s.FaultMaxNanos, attrs)
			o.ObserveInt64(readPageMaxNanos, s.ReadPageMaxNanos, attrs)
			o.ObserveInt64(backingReadMaxNanos, s.BackingReadMaxNanos, attrs)
			o.ObserveInt64(copyMaxNanos, s.CopyMaxNanos, attrs)
			return nil
		},
		cacheHits, cacheMisses, faults, backingBytesRead, copies, copyErrors,
		cacheLookupNanos, cacheAddNanos, faultNanos, readPageNanos, backingReadNanos, copyNanos,
		cacheBytes, cacheMaxBytes, cacheItems, cacheShards, activeSessions, activeFaults,
		maxConcurrentFaults, draining,
		cacheLookupMaxNanos, cacheAddMaxNanos, faultMaxNanos, readPageMaxNanos, backingReadMaxNanos, copyMaxNanos,
	)
	if err != nil {
		return fmt.Errorf("register uffd metrics callback: %w", err)
	}
	return nil
}

func boolToInt64(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
