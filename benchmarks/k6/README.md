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
  HYPEMAN_BENCH_MAX_VUS=16
```

The Make target writes:

- `.bench/k6/activity-ramp.html`
- `.bench/k6/activity-ramp-summary.json`

The benchmark creates a shared ingress named `bench-activity-ramp` if one does not already exist. By default it listens on host port `8081`, matches `{instance}.hypeman-bench.local`, and targets port `80` on each instance. Override the probe path with:

```sh
HYPEMAN_PROBE_URL=http://host:8081/
HYPEMAN_PROBE_HOST_SUFFIX=.hypeman-bench.local
HYPEMAN_INGRESS_HOST_PORT=8081
HYPEMAN_INGRESS_TARGET_PORT=80
```

Capacity rejections from `POST /instances` are recorded as `hypeman_create_rejected` and `hypeman_create_rejections`. They are not treated as unexpected script errors because they identify the concurrency level where the server starts refusing new activity.
