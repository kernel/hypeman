# k6 benchmarks

This directory contains TypeScript k6 benchmarks for a running Hypeman API.

## Activity ramp

`activity-ramp.ts` runs a closed workload where each virtual user repeatedly:

1. creates an instance from a ready image,
2. waits for it to reach `Running`,
3. sends an HTTP probe through a shared pattern ingress,
4. deletes the instance.

The default ramp increases concurrency by one virtual user every two minutes up to 16 virtual users. Tune the run with environment variables:

```sh
export HYPEMAN_API_KEY=...
make bench-activity-ramp \
  HYPEMAN_BASE_URL=http://127.0.0.1:8080 \
  HYPEMAN_IMAGE=docker.io/library/nginx:alpine \
  HYPEMAN_HYPERVISOR=cloud-hypervisor \
  HYPEMAN_BENCH_MAX_VUS=16
```

The Make target writes:

- `.bench/k6/activity-ramp.html`
- `.bench/k6/activity-ramp-summary.json`

The Make target uses 120-second dashboard buckets by default so each HTML report point lines up with one default ramp window. Override that with `HYPEMAN_BENCH_DASHBOARD_PERIOD`.

The benchmark creates a shared ingress named `bench-activity-ramp` if one does not already exist. By default it listens on host port `80`, matches `{instance}.hypeman-bench.local`, and targets port `80` on each instance. Override the probe path with:

```sh
HYPEMAN_PROBE_URL=http://host/
HYPEMAN_PROBE_HOST_SUFFIX=.hypeman-bench.local
HYPEMAN_INGRESS_HOST_PORT=80
HYPEMAN_INGRESS_TARGET_PORT=80
```

Capacity rejections from `POST /instances` are recorded as `hypeman_create_rejected` and `hypeman_create_rejections`. They are not treated as unexpected script errors because they identify the concurrency level where the server starts refusing new activity.
Rejected creates back off for one second by default so a saturated server does not produce a tight 409 loop. Override that with `HYPEMAN_CREATE_REJECTED_BACKOFF_SECONDS`.

Set `HYPEMAN_HYPERVISOR` to `cloud-hypervisor`, `firecracker`, or `qemu` to run the same activity loop against a specific hypervisor. The value is sent on instance creation and added as a metric tag.
