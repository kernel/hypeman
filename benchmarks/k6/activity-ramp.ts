import { check, fail, sleep } from 'k6';
import exec from 'k6/execution';
import http, { RefinedResponse, ResponseType } from 'k6/http';
import { Counter, Rate, Trend } from 'k6/metrics';

type Tags = Record<string, string>;

interface Config {
  baseUrl: string;
  apiKey: string;
  image: string;
  runId: string;
  startVUs: number;
  maxVUs: number;
  vuStep: number;
  stageDuration: string;
  gracefulRampDown: string;
  instanceMemory: string;
  instanceOverlaySize: string;
  instanceVCPUs: number;
  waitTimeoutSeconds: number;
  probeUrl: string;
  probePath: string;
  probeHostSuffix: string;
  probeAttempts: number;
  probeIntervalSeconds: number;
  ingressName: string;
  ingressHostPattern: string;
  ingressHostPort: number;
  ingressTargetPort: number;
  imageReadyTimeoutSeconds: number;
}

const createMs = new Trend('hypeman_create_instance_ms', true);
const waitRunningMs = new Trend('hypeman_wait_running_ms', true);
const probeReadyMs = new Trend('hypeman_probe_ready_ms', true);
const probeHTTPMs = new Trend('hypeman_probe_http_ms', true);
const deleteMs = new Trend('hypeman_delete_instance_ms', true);
const activityMs = new Trend('hypeman_activity_total_ms', true);
const activityOk = new Rate('hypeman_activity_ok');
const cleanupOk = new Rate('hypeman_cleanup_ok');
const createRejected = new Rate('hypeman_create_rejected');
const createRejections = new Counter('hypeman_create_rejections');
const probeOk = new Rate('hypeman_probe_ok');

const config = loadConfig();

export const options = {
  setupTimeout: '15m',
  teardownTimeout: '10m',
  scenarios: {
    activity_ramp: {
      executor: 'ramping-vus',
      startVUs: config.startVUs,
      stages: rampStages(config),
      gracefulRampDown: config.gracefulRampDown,
    },
  },
  thresholds: {
    hypeman_cleanup_ok: ['rate>0.95'],
    hypeman_probe_ok: ['rate>0.80'],
  },
};

export function setup() {
  checkRequiredConfig(config);
  ensureHealthy();
  ensureImageReady(config.image);
  ensurePatternIngress();

  return {
    runId: config.runId,
  };
}

export default function (data: { runId: string }) {
  const iterationStart = Date.now();
  const instanceName = instanceNameFor(data.runId);
  const tags: Tags = {
    benchmark: 'activity-ramp',
    run_id: data.runId,
    instance: instanceName,
  };
  let ok = false;
  let created = false;

  try {
    created = createInstance(instanceName, tags);
    if (!created) {
      return;
    }
    waitForRunning(instanceName, tags);
    probeInstance(instanceName, tags);
    ok = true;
  } finally {
    if (created) {
      const deleted = deleteInstance(instanceName, tags);
      cleanupOk.add(deleted, tags);
    }
    activityOk.add(ok, tags);
    activityMs.add(Date.now() - iterationStart, tags);
  }
}

export function teardown(data: { runId: string }) {
  cleanupRunInstances(data.runId);
}

function loadConfig(): Config {
  const baseUrl = trimRight(requiredEnv('HYPEMAN_BASE_URL', 'http://127.0.0.1:8080'), '/');
  const ingressHostPort = intEnv('HYPEMAN_INGRESS_HOST_PORT', 8081);

  return {
    baseUrl,
    apiKey: requiredEnv('HYPEMAN_API_KEY', ''),
    image: requiredEnv('HYPEMAN_IMAGE', 'docker.io/library/nginx:alpine'),
    runId: envString('HYPEMAN_BENCH_RUN_ID', defaultRunId()),
    startVUs: intEnv('HYPEMAN_BENCH_START_VUS', 1),
    maxVUs: intEnv('HYPEMAN_BENCH_MAX_VUS', 16),
    vuStep: intEnv('HYPEMAN_BENCH_VU_STEP', 1),
    stageDuration: envString('HYPEMAN_BENCH_STAGE_DURATION', '2m'),
    gracefulRampDown: envString('HYPEMAN_BENCH_GRACEFUL_RAMP_DOWN', '10m'),
    instanceMemory: envString('HYPEMAN_INSTANCE_MEMORY', '512MB'),
    instanceOverlaySize: envString('HYPEMAN_INSTANCE_OVERLAY_SIZE', '2GB'),
    instanceVCPUs: intEnv('HYPEMAN_INSTANCE_VCPUS', 1),
    waitTimeoutSeconds: durationSeconds(envString('HYPEMAN_WAIT_TIMEOUT', '5m')),
    probeUrl: trimRight(envString('HYPEMAN_PROBE_URL', probeURLFromBaseURL(baseUrl, ingressHostPort)), '/'),
    probePath: envString('HYPEMAN_PROBE_PATH', '/'),
    probeHostSuffix: envString('HYPEMAN_PROBE_HOST_SUFFIX', '.hypeman-bench.local'),
    probeAttempts: intEnv('HYPEMAN_PROBE_ATTEMPTS', 30),
    probeIntervalSeconds: floatEnv('HYPEMAN_PROBE_INTERVAL_SECONDS', 1),
    ingressName: envString('HYPEMAN_INGRESS_NAME', 'bench-activity-ramp'),
    ingressHostPattern: envString('HYPEMAN_INGRESS_HOST_PATTERN', '{instance}.hypeman-bench.local'),
    ingressHostPort,
    ingressTargetPort: intEnv('HYPEMAN_INGRESS_TARGET_PORT', 80),
    imageReadyTimeoutSeconds: intEnv('HYPEMAN_IMAGE_READY_TIMEOUT_SECONDS', 600),
  };
}

function rampStages(cfg: Config): Array<{ duration: string; target: number }> {
  const stages: Array<{ duration: string; target: number }> = [];
  for (let target = cfg.startVUs + cfg.vuStep; target <= cfg.maxVUs; target += cfg.vuStep) {
    stages.push({ duration: cfg.stageDuration, target });
  }
  if (stages.length === 0 || stages[stages.length - 1].target !== cfg.maxVUs) {
    stages.push({ duration: cfg.stageDuration, target: cfg.maxVUs });
  }
  stages.push({ duration: cfg.stageDuration, target: 0 });
  return stages;
}

function checkRequiredConfig(cfg: Config) {
  if (!cfg.apiKey) {
    fail('HYPEMAN_API_KEY is required');
  }
  if (cfg.maxVUs < cfg.startVUs) {
    fail('HYPEMAN_BENCH_MAX_VUS must be greater than or equal to HYPEMAN_BENCH_START_VUS');
  }
  if (cfg.vuStep < 1) {
    fail('HYPEMAN_BENCH_VU_STEP must be at least 1');
  }
}

function ensureHealthy() {
  const res = apiGet('/health', { kind: 'setup', step: 'health' });
  assertStatus(res, [200], 'health check');
}

function ensureImageReady(image: string) {
  const encoded = encodeURIComponent(image);
  let res = apiGet(`/images/${encoded}`, { kind: 'setup', step: 'image-get' });
  if (res.status === 404) {
    res = apiPost('/images', { name: image }, { kind: 'setup', step: 'image-create' });
    assertStatus(res, [202], 'create image');
  } else {
    assertStatus(res, [200], 'get image');
  }

  const deadline = Date.now() + config.imageReadyTimeoutSeconds * 1000;
  while (Date.now() < deadline) {
    const imageRes = apiGet(`/images/${encoded}`, { kind: 'setup', step: 'image-poll' });
    assertStatus(imageRes, [200], 'poll image');
    const imageBody = imageRes.json() as { status?: string; error?: string };

    if (imageBody.status === 'ready') {
      return;
    }
    if (imageBody.status === 'failed') {
      fail(`image ${image} failed to become ready: ${imageBody.error || 'unknown error'}`);
    }
    sleep(2);
  }

  fail(`image ${image} did not become ready before ${config.imageReadyTimeoutSeconds}s`);
}

function ensurePatternIngress() {
  const encoded = encodeURIComponent(config.ingressName);
  const existing = apiGet(`/ingresses/${encoded}`, { kind: 'setup', step: 'ingress-get' });
  if (existing.status === 200) {
    return;
  }
  if (existing.status !== 404) {
    assertStatus(existing, [200, 404], 'get ingress');
  }

  const created = apiPost('/ingresses', {
    name: config.ingressName,
    tags: { benchmark: 'activity-ramp' },
    rules: [{
      match: {
        hostname: config.ingressHostPattern,
        port: config.ingressHostPort,
      },
      target: {
        instance: '{instance}',
        port: config.ingressTargetPort,
      },
      tls: false,
      redirect_http: false,
    }],
  }, { kind: 'setup', step: 'ingress-create' });

  assertStatus(created, [201, 409], 'create ingress');
}

function createInstance(name: string, tags: Tags): boolean {
  const started = Date.now();
  const res = apiPost('/instances', {
    name,
    image: config.image,
    size: config.instanceMemory,
    overlay_size: config.instanceOverlaySize,
    vcpus: config.instanceVCPUs,
    network: { enabled: true },
    tags: {
      benchmark: 'activity-ramp',
      run_id: config.runId,
    },
    skip_kernel_headers: true,
  }, tagStep(tags, 'create'));

  createMs.add(Date.now() - started, tags);
  check(res, {
    [`create instance ${name} accepted or capacity-rejected`]: (r) => r.status === 201 || r.status === 409,
  });
  if (res.status === 201) {
    createRejected.add(false, tags);
    return true;
  }
  if (res.status === 409) {
    createRejected.add(true, tags);
    createRejections.add(1, tags);
    return false;
  }

  fail(`create instance ${name} failed with status ${res.status}: ${String(res.body).slice(0, 500)}`);
}

function waitForRunning(name: string, tags: Tags) {
  const started = Date.now();
  const path = `/instances/${encodeURIComponent(name)}`;
  const deadline = started + config.waitTimeoutSeconds * 1000;

  while (Date.now() < deadline) {
    const res = apiGet(path, tagStep(tags, 'wait-running'));
    assertStatus(res, [200], `get instance ${name}`);

    const body = res.json() as { state?: string; state_error?: string | null };
    if (body.state === 'Running') {
      waitRunningMs.add(Date.now() - started, tags);
      return;
    }
    if (body.state === 'Stopped' || body.state === 'Standby' || body.state === 'Shutdown' || body.state === 'Unknown') {
      fail(`instance ${name} reached terminal state while waiting: state=${body.state} error=${body.state_error || ''}`);
    }
    sleep(1);
  }

  waitRunningMs.add(Date.now() - started, tags);
  fail(`instance ${name} did not reach Running before ${config.waitTimeoutSeconds}s`);
}

function probeInstance(name: string, tags: Tags) {
  const started = Date.now();
  const probeURL = `${config.probeUrl}${config.probePath.startsWith('/') ? config.probePath : `/${config.probePath}`}`;
  const host = `${name}${config.probeHostSuffix}`;

  for (let attempt = 1; attempt <= config.probeAttempts; attempt += 1) {
    const res = http.get(probeURL, {
      headers: { Host: host },
      timeout: '30s',
      tags: tagStep(tags, 'probe'),
    });
    probeHTTPMs.add(res.timings.duration, tags);

    if (res.status >= 200 && res.status < 500) {
      probeReadyMs.add(Date.now() - started, tags);
      probeOk.add(true, tags);
      return;
    }

    if (attempt < config.probeAttempts) {
      sleep(config.probeIntervalSeconds);
    }
  }

  probeOk.add(false, tags);
  fail(`instance ${name} did not answer HTTP probe after ${config.probeAttempts} attempts`);
}

function deleteInstance(name: string, tags: Tags): boolean {
  const started = Date.now();
  const res = apiDelete(`/instances/${encodeURIComponent(name)}`, tagStep(tags, 'delete'));
  deleteMs.add(Date.now() - started, tags);
  return res.status === 204 || res.status === 404;
}

function cleanupRunInstances(runId: string) {
  const query = `tags%5Bbenchmark%5D=activity-ramp&tags%5Brun_id%5D=${encodeURIComponent(runId)}`;
  const res = apiGet(`/instances?${query}`, { kind: 'teardown', step: 'list-run-instances', run_id: runId });
  if (res.status !== 200) {
    return;
  }

  const instances = res.json() as Array<{ id?: string; name?: string }>;
  for (const instance of instances) {
    const ref = instance.name || instance.id;
    if (!ref) {
      continue;
    }
    const deleted = deleteInstance(ref, {
      benchmark: 'activity-ramp',
      run_id: runId,
      instance: ref,
      kind: 'teardown',
    });
    cleanupOk.add(deleted, {
      benchmark: 'activity-ramp',
      run_id: runId,
      instance: ref,
      kind: 'teardown',
    });
  }
}

function apiGet(path: string, tags: Tags): RefinedResponse<ResponseType | undefined> {
  return http.get(`${config.baseUrl}${path}`, {
    headers: authHeaders(),
    timeout: '10m',
    tags,
  });
}

function apiPost(path: string, body: unknown, tags: Tags): RefinedResponse<ResponseType | undefined> {
  return http.post(`${config.baseUrl}${path}`, JSON.stringify(body), {
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    timeout: '10m',
    tags,
  });
}

function apiDelete(path: string, tags: Tags): RefinedResponse<ResponseType | undefined> {
  return http.del(`${config.baseUrl}${path}`, undefined, {
    headers: authHeaders(),
    timeout: '10m',
    tags,
  });
}

function authHeaders(): Record<string, string> {
  return {
    Authorization: `Bearer ${config.apiKey}`,
  };
}

function assertStatus(res: RefinedResponse<ResponseType | undefined>, allowed: number[], label: string) {
  const ok = check(res, {
    [`${label} status ${allowed.join('/')}`]: (r) => allowed.includes(r.status),
  });
  if (!ok) {
    fail(`${label} failed with status ${res.status}: ${String(res.body).slice(0, 500)}`);
  }
}

function tagStep(tags: Tags, step: string): Tags {
  return { ...tags, step };
}

function instanceNameFor(runId: string): string {
  const vu = exec.vu.idInTest;
  const iter = exec.scenario.iterationInTest;
  return `hm-bench-${runId}-${vu}-${iter}`.slice(0, 63).replace(/-+$/, '');
}

function defaultRunId(): string {
  return Math.floor(Date.now() / 1000).toString(36);
}

function probeURLFromBaseURL(baseUrl: string, port: number): string {
  const match = baseUrl.match(/^(https?):\/\/([^/:]+)(?::[0-9]+)?/);
  if (!match) {
    return `http://127.0.0.1:${port}`;
  }
  return `${match[1]}://${match[2]}:${port}`;
}

function envString(name: string, fallback: string): string {
  const value = __ENV[name];
  return value === undefined || value === '' ? fallback : value;
}

function requiredEnv(name: string, fallback: string): string {
  return envString(name, fallback);
}

function intEnv(name: string, fallback: number): number {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') {
    return fallback;
  }
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed)) {
    fail(`${name} must be an integer`);
  }
  return parsed;
}

function floatEnv(name: string, fallback: number): number {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') {
    return fallback;
  }
  const parsed = Number.parseFloat(raw);
  if (!Number.isFinite(parsed)) {
    fail(`${name} must be a number`);
  }
  return parsed;
}

function durationSeconds(value: string): number {
  const match = value.match(/^([0-9]+(?:\.[0-9]+)?)(ms|s|m|h)?$/);
  if (!match) {
    fail(`duration must be a number with optional ms, s, m, or h suffix: ${value}`);
  }
  const amount = Number.parseFloat(match[1]);
  const unit = match[2] || 's';
  switch (unit) {
    case 'ms':
      return amount / 1000;
    case 's':
      return amount;
    case 'm':
      return amount * 60;
    case 'h':
      return amount * 60 * 60;
    default:
      fail(`unsupported duration unit: ${unit}`);
  }
}

function trimRight(value: string, suffix: string): string {
  let out = value;
  while (out.endsWith(suffix)) {
    out = out.slice(0, -suffix.length);
  }
  return out;
}
