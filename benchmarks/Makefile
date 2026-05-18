SHELL := /bin/bash

.PHONY: bench-activity-ramp

REPO_ROOT := $(abspath $(CURDIR)/..)

K6 ?= k6
K6_OUT_DIR ?= $(REPO_ROOT)/.bench/k6
HYPEMAN_BASE_URL ?= http://127.0.0.1:8080
HYPEMAN_IMAGE ?= docker.io/library/nginx:alpine
HYPEMAN_BENCH_MAX_VUS ?= 16
HYPEMAN_BENCH_VU_STEP ?= 1
HYPEMAN_BENCH_STAGE_DURATION ?= 2m
HYPEMAN_BENCH_DASHBOARD_PERIOD ?= 120s
HYPEMAN_HYPERVISOR ?= cloud-hypervisor
HYPEMAN_INGRESS_HOST_PORT ?= 80
HYPEMAN_CREATE_REJECTED_BACKOFF_SECONDS ?= 1

bench-activity-ramp:
	@if ! command -v $(K6) >/dev/null 2>&1; then \
		echo "k6 not found; install k6 or run with K6=/path/to/k6"; \
		exit 1; \
	fi
	@if [ -z "$$HYPEMAN_API_KEY" ]; then \
		echo "HYPEMAN_API_KEY is required"; \
		exit 1; \
	fi
	@mkdir -p $(K6_OUT_DIR)
	K6_WEB_DASHBOARD=true \
	K6_WEB_DASHBOARD_PORT=-1 \
	K6_WEB_DASHBOARD_PERIOD=$(HYPEMAN_BENCH_DASHBOARD_PERIOD) \
	K6_WEB_DASHBOARD_EXPORT=$(K6_OUT_DIR)/activity-ramp.html \
	$(K6) run \
		--summary-mode=full \
		--summary-trend-stats="avg,med,p(90),p(95),p(99),min,max" \
		--summary-export=$(K6_OUT_DIR)/activity-ramp-summary.json \
		-e HYPEMAN_BASE_URL="$(HYPEMAN_BASE_URL)" \
		-e HYPEMAN_API_KEY="$$HYPEMAN_API_KEY" \
		-e HYPEMAN_IMAGE="$(HYPEMAN_IMAGE)" \
		-e HYPEMAN_HYPERVISOR="$(HYPEMAN_HYPERVISOR)" \
		-e HYPEMAN_BENCH_MAX_VUS="$(HYPEMAN_BENCH_MAX_VUS)" \
		-e HYPEMAN_BENCH_VU_STEP="$(HYPEMAN_BENCH_VU_STEP)" \
		-e HYPEMAN_BENCH_STAGE_DURATION="$(HYPEMAN_BENCH_STAGE_DURATION)" \
		-e HYPEMAN_INGRESS_HOST_PORT="$(HYPEMAN_INGRESS_HOST_PORT)" \
		-e HYPEMAN_CREATE_REJECTED_BACKOFF_SECONDS="$(HYPEMAN_CREATE_REJECTED_BACKOFF_SECONDS)" \
		k6/activity-ramp.ts
