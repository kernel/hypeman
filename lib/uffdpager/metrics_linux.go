//go:build linux

package uffdpager

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RegisterMetrics installs observable OTel instruments backed by the pager's
// existing atomic counters. Instruments are read from the snapshot returned by
// statsFn on each collection cycle, so no new state is introduced.
//
// versionKey is attached to every observation so multi-version pager
// deployments can be distinguished at query time.
func RegisterMetrics(meter metric.Meter, versionKey string, statsFn func() Stats) error {
	if meter == nil || statsFn == nil {
		return nil
	}

	cacheHits, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_hits_total",
		metric.WithDescription("Total UFFD page cache hits"),
	)
	if err != nil {
		return fmt.Errorf("create cache hits counter: %w", err)
	}
	cacheMisses, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_misses_total",
		metric.WithDescription("Total UFFD page cache misses (page read from backing file)"),
	)
	if err != nil {
		return fmt.Errorf("create cache misses counter: %w", err)
	}
	faults, err := meter.Int64ObservableCounter(
		"hypeman_uffd_faults_total",
		metric.WithDescription("Total UFFD page faults handled"),
	)
	if err != nil {
		return fmt.Errorf("create faults counter: %w", err)
	}
	backingBytesRead, err := meter.Int64ObservableCounter(
		"hypeman_uffd_backing_bytes_read_total",
		metric.WithDescription("Total bytes read from the backing memory file on cache miss"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create backing bytes read counter: %w", err)
	}
	copies, err := meter.Int64ObservableCounter(
		"hypeman_uffd_copies_total",
		metric.WithDescription("Total UFFDIO_COPY operations issued"),
	)
	if err != nil {
		return fmt.Errorf("create copies counter: %w", err)
	}
	copyErrors, err := meter.Int64ObservableCounter(
		"hypeman_uffd_copy_errors_total",
		metric.WithDescription("Total UFFDIO_COPY operations that returned an error"),
	)
	if err != nil {
		return fmt.Errorf("create copy errors counter: %w", err)
	}
	cacheLookupNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_lookup_nanos_total",
		metric.WithDescription("Total nanoseconds spent in cache lookups (sum across all faults)"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache lookup nanos counter: %w", err)
	}
	cacheAddNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_cache_add_nanos_total",
		metric.WithDescription("Total nanoseconds spent inserting entries into the cache"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache add nanos counter: %w", err)
	}
	faultNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_fault_nanos_total",
		metric.WithDescription("Total nanoseconds spent handling faults end-to-end"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create fault nanos counter: %w", err)
	}
	readPageNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_read_page_nanos_total",
		metric.WithDescription("Total nanoseconds spent reading pages (cache lookup + backing read)"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create read page nanos counter: %w", err)
	}
	backingReadNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_backing_read_nanos_total",
		metric.WithDescription("Total nanoseconds spent reading pages from the backing file"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create backing read nanos counter: %w", err)
	}
	copyNanos, err := meter.Int64ObservableCounter(
		"hypeman_uffd_copy_nanos_total",
		metric.WithDescription("Total nanoseconds spent in UFFDIO_COPY calls"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create copy nanos counter: %w", err)
	}

	cacheBytes, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_bytes",
		metric.WithDescription("Current UFFD page cache size in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create cache bytes gauge: %w", err)
	}
	cacheMaxBytes, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_max_bytes",
		metric.WithDescription("Configured UFFD page cache capacity in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("create cache max bytes gauge: %w", err)
	}
	cacheItems, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_items",
		metric.WithDescription("Number of pages currently held in the UFFD cache"),
	)
	if err != nil {
		return fmt.Errorf("create cache items gauge: %w", err)
	}
	cacheShards, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_shards",
		metric.WithDescription("Number of shards in the UFFD page cache"),
	)
	if err != nil {
		return fmt.Errorf("create cache shards gauge: %w", err)
	}
	activeSessions, err := meter.Int64ObservableGauge(
		"hypeman_uffd_active_sessions",
		metric.WithDescription("Number of active pager sessions"),
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
		metric.WithDescription("High-water mark of concurrent in-flight faults since process start"),
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
		metric.WithDescription("High-water mark of cache lookup latency since process start"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache lookup max nanos gauge: %w", err)
	}
	cacheAddMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_cache_add_max_nanos",
		metric.WithDescription("High-water mark of cache add latency since process start"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create cache add max nanos gauge: %w", err)
	}
	faultMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_fault_max_nanos",
		metric.WithDescription("High-water mark of end-to-end fault handling latency since process start"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create fault max nanos gauge: %w", err)
	}
	readPageMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_read_page_max_nanos",
		metric.WithDescription("High-water mark of page read latency since process start"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create read page max nanos gauge: %w", err)
	}
	backingReadMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_backing_read_max_nanos",
		metric.WithDescription("High-water mark of backing file read latency since process start"),
		metric.WithUnit("ns"),
	)
	if err != nil {
		return fmt.Errorf("create backing read max nanos gauge: %w", err)
	}
	copyMaxNanos, err := meter.Int64ObservableGauge(
		"hypeman_uffd_copy_max_nanos",
		metric.WithDescription("High-water mark of UFFDIO_COPY latency since process start"),
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
