package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type config struct {
	baseURL           string
	apiKey            string
	sourceInstance    string
	sourceNameRegex   string
	iterations        int
	waitForNetwork    bool
	targetState       string
	namePrefix        string
	cdpHostSuffix     string
	cdpScheme         string
	cdpPublicPort     int
	cdpTargetPort     int
	cdpDirectIP       bool
	createCDPIngress  bool
	cleanupIngress    bool
	cleanupInstances  bool
	pollInterval      time.Duration
	timeout           time.Duration
	insecureTLS       bool
	printRawIteration bool
}

type apiClient struct {
	base   *url.URL
	key    string
	client *http.Client
}

type apiInstance struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	State       string            `json:"state"`
	Image       string            `json:"image"`
	Hypervisor  string            `json:"hypervisor"`
	Size        string            `json:"size"`
	VCPUs       int               `json:"vcpus"`
	HasSnapshot bool              `json:"has_snapshot"`
	Env         map[string]string `json:"env"`
	Network     *struct {
		IP string `json:"ip"`
	} `json:"network"`
}

type apiIngress struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Tags  map[string]string `json:"tags"`
	Rules []ingressRule     `json:"rules"`
}

type ingressRule struct {
	Match struct {
		Hostname string `json:"hostname"`
		Port     *int   `json:"port,omitempty"`
	} `json:"match"`
	Target struct {
		Instance string `json:"instance"`
		Port     int    `json:"port"`
	} `json:"target"`
	TLS          *bool `json:"tls,omitempty"`
	RedirectHTTP *bool `json:"redirect_http,omitempty"`
}

type cdpVersion struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type iterationResult struct {
	Iteration             int     `json:"iteration"`
	InstanceID            string  `json:"instance_id"`
	InstanceName          string  `json:"instance_name"`
	InstanceState         string  `json:"instance_state"`
	NetworkIP             string  `json:"network_ip,omitempty"`
	ForkMs                float64 `json:"fork_ms"`
	CDPJSONAfterForkMs    float64 `json:"cdp_json_after_fork_ms"`
	CDPGetVersionAfterMs  float64 `json:"cdp_get_version_after_json_ms"`
	TotalToBrowserReadyMs float64 `json:"total_to_browser_ready_ms"`
	Browser               string  `json:"browser,omitempty"`
	ProtocolVersion       string  `json:"protocol_version,omitempty"`
	Error                 string  `json:"error,omitempty"`
}

func main() {
	cfg := parseConfig()
	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "browser cold-start bench failed: %v\n", err)
		os.Exit(1)
	}
}

func parseConfig() config {
	cfg := config{}
	flag.StringVar(&cfg.baseURL, "base-url", envString("HYPEMAN_BASE_URL", ""), "Hypeman API base URL")
	flag.StringVar(&cfg.apiKey, "api-key", envString("HYPEMAN_API_KEY", ""), "Hypeman API bearer token")
	flag.StringVar(&cfg.sourceInstance, "source-instance", envString("HYPEMAN_BROWSER_SOURCE_INSTANCE_ID", ""), "standby source instance ID/name to fork")
	flag.StringVar(&cfg.sourceNameRegex, "source-name-regex", envString("HYPEMAN_BROWSER_SOURCE_NAME_REGEX", ""), "optional regexp for auto-discovered source instance names")
	flag.IntVar(&cfg.iterations, "iterations", envInt("HYPEMAN_BROWSER_COLD_START_ITERS", 5), "number of forks to benchmark")
	flag.BoolVar(&cfg.waitForNetwork, "wait-for-network", envBool("HYPEMAN_BROWSER_WAIT_FOR_NETWORK", true), "ask fork API to wait for guest network before returning")
	flag.StringVar(&cfg.targetState, "target-state", envString("HYPEMAN_BROWSER_TARGET_STATE", "Running"), "fork target_state")
	flag.StringVar(&cfg.namePrefix, "name-prefix", envString("HYPEMAN_BROWSER_NAME_PREFIX", "bench-browser"), "forked instance name prefix")
	flag.StringVar(&cfg.cdpHostSuffix, "cdp-host-suffix", envString("HYPEMAN_BROWSER_CDP_HOST_SUFFIX", ""), "CDP ingress hostname suffix, e.g. dev-yul-hypeman-1.kernel.sh")
	flag.StringVar(&cfg.cdpScheme, "cdp-scheme", envString("HYPEMAN_BROWSER_CDP_SCHEME", ""), "CDP scheme: https or http; defaults to http with -cdp-direct-ip, otherwise https")
	flag.IntVar(&cfg.cdpPublicPort, "cdp-public-port", envInt("HYPEMAN_BROWSER_CDP_PUBLIC_PORT", 9222), "external CDP ingress port")
	flag.IntVar(&cfg.cdpTargetPort, "cdp-target-port", envInt("HYPEMAN_BROWSER_CDP_TARGET_PORT", 9222), "guest CDP target port")
	flag.BoolVar(&cfg.cdpDirectIP, "cdp-direct-ip", envBool("HYPEMAN_BROWSER_CDP_DIRECT_IP", false), "connect directly to the forked VM network IP instead of using ingress hostnames")
	flag.BoolVar(&cfg.createCDPIngress, "create-cdp-ingress", envBool("HYPEMAN_BROWSER_CREATE_CDP_INGRESS", false), "create a temporary wildcard ingress for CDP before measuring")
	flag.BoolVar(&cfg.cleanupIngress, "cleanup-ingress", envBool("HYPEMAN_BROWSER_CLEANUP_INGRESS", true), "delete temporary CDP ingress after the run")
	flag.BoolVar(&cfg.cleanupInstances, "cleanup-instances", envBool("HYPEMAN_BROWSER_CLEANUP_INSTANCES", true), "delete forked benchmark instances after each iteration")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", envDuration("HYPEMAN_BROWSER_CDP_POLL_INTERVAL", 20*time.Millisecond), "CDP readiness poll interval")
	flag.DurationVar(&cfg.timeout, "timeout", envDuration("HYPEMAN_BROWSER_CDP_TIMEOUT", 60*time.Second), "per-iteration timeout")
	flag.BoolVar(&cfg.insecureTLS, "insecure-tls", envBool("HYPEMAN_BROWSER_INSECURE_TLS", false), "skip TLS verification for benchmark HTTP/WebSocket clients")
	flag.BoolVar(&cfg.printRawIteration, "print-raw-iteration", envBool("HYPEMAN_BROWSER_PRINT_RAW_ITERATION", false), "print each iteration as JSON as it completes")
	flag.Parse()

	if cfg.baseURL == "" {
		fail("missing -base-url or HYPEMAN_BASE_URL")
	}
	if cfg.apiKey == "" {
		fail("missing -api-key or HYPEMAN_API_KEY")
	}
	if cfg.iterations <= 0 {
		fail("-iterations must be positive")
	}
	if cfg.pollInterval <= 0 {
		fail("-poll-interval must be positive")
	}
	if cfg.timeout <= 0 {
		fail("-timeout must be positive")
	}
	if cfg.cdpHostSuffix == "" && !cfg.cdpDirectIP {
		if u, err := url.Parse(cfg.baseURL); err == nil {
			cfg.cdpHostSuffix = strings.TrimPrefix(u.Hostname(), "hypeman.")
		}
	}
	cfg.cdpHostSuffix = strings.TrimPrefix(strings.TrimSpace(cfg.cdpHostSuffix), ".")
	if cfg.cdpHostSuffix == "" && !cfg.cdpDirectIP {
		fail("missing -cdp-host-suffix or HYPEMAN_BROWSER_CDP_HOST_SUFFIX")
	}
	cfg.cdpScheme = strings.ToLower(strings.TrimSpace(cfg.cdpScheme))
	if cfg.cdpScheme == "" {
		if cfg.cdpDirectIP {
			cfg.cdpScheme = "http"
		} else {
			cfg.cdpScheme = "https"
		}
	}
	if cfg.cdpScheme != "https" && cfg.cdpScheme != "http" {
		fail("-cdp-scheme must be https or http")
	}
	if cfg.cdpDirectIP && cfg.createCDPIngress {
		fail("-cdp-direct-ip and -create-cdp-ingress cannot be combined")
	}
	return cfg
}

func run(ctx context.Context, cfg config) error {
	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}

	source, err := resolveSource(ctx, client, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("source: id=%s name=%s image=%s hypervisor=%s size=%s vcpus=%d\n",
		source.ID, source.Name, source.Image, source.Hypervisor, source.Size, source.VCPUs)

	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	var createdIngressID string
	if cfg.createCDPIngress {
		id, created, err := ensureCDPIngress(ctx, client, cfg, runID)
		if err != nil {
			return err
		}
		if created {
			createdIngressID = id
			fmt.Printf("created temporary CDP ingress: id=%s host=*.%s port=%d target_port=%d\n",
				id, cfg.cdpHostSuffix, cfg.cdpPublicPort, cfg.cdpTargetPort)
			defer func() {
				if cfg.cleanupIngress {
					if err := client.do(context.Background(), http.MethodDelete, "/ingresses/"+url.PathEscape(createdIngressID), nil, nil); err != nil {
						fmt.Fprintf(os.Stderr, "warning: delete cdp ingress %s: %v\n", createdIngressID, err)
					}
				}
			}()
		} else {
			fmt.Printf("using existing CDP ingress: id=%s host=*.%s port=%d target_port=%d\n",
				id, cfg.cdpHostSuffix, cfg.cdpPublicPort, cfg.cdpTargetPort)
		}
	}

	results := make([]iterationResult, 0, cfg.iterations)
	for i := 1; i <= cfg.iterations; i++ {
		res := runIteration(ctx, client, cfg, source.ID, runID, i)
		results = append(results, res)
		if cfg.printRawIteration {
			printJSON(res)
		} else if res.Error != "" {
			fmt.Printf("[%d/%d] ERROR total=%.1fms fork=%.1fms cdp_json=%.1fms cdp_ws=%.1fms err=%s\n",
				i, cfg.iterations, res.TotalToBrowserReadyMs, res.ForkMs, res.CDPJSONAfterForkMs, res.CDPGetVersionAfterMs, res.Error)
		} else {
			fmt.Printf("[%d/%d] total=%.1fms fork=%.1fms cdp_json=%.1fms cdp_ws=%.1fms browser=%s instance=%s\n",
				i, cfg.iterations, res.TotalToBrowserReadyMs, res.ForkMs, res.CDPJSONAfterForkMs, res.CDPGetVersionAfterMs, res.Browser, res.InstanceName)
		}
	}

	printSummary(results)
	return nil
}

func runIteration(parent context.Context, client *apiClient, cfg config, sourceID string, runID string, iter int) iterationResult {
	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()

	name := benchmarkName(cfg.namePrefix, runID, iter)
	res := iterationResult{Iteration: iter, InstanceName: name}
	totalStart := time.Now()

	body := map[string]any{
		"name":             name,
		"target_state":     cfg.targetState,
		"wait_for_network": cfg.waitForNetwork,
	}
	var inst apiInstance
	forkStart := time.Now()
	if err := client.do(ctx, http.MethodPost, "/instances/"+url.PathEscape(sourceID)+"/fork", body, &inst); err != nil {
		res.ForkMs = msSince(forkStart)
		res.TotalToBrowserReadyMs = msSince(totalStart)
		res.Error = fmt.Sprintf("fork: %v", err)
		return res
	}
	forkDone := time.Now()
	res.ForkMs = ms(forkDone.Sub(forkStart))
	res.InstanceID = inst.ID
	res.InstanceName = inst.Name
	res.InstanceState = inst.State
	if inst.Network != nil {
		res.NetworkIP = inst.Network.IP
	}
	if cfg.cleanupInstances && inst.ID != "" {
		defer func() {
			if err := client.do(context.Background(), http.MethodDelete, "/instances/"+url.PathEscape(inst.ID), nil, nil); err != nil {
				fmt.Fprintf(os.Stderr, "warning: delete instance %s (%s): %v\n", inst.Name, inst.ID, err)
			}
		}()
	}

	version, versionURL, err := waitForCDPVersion(ctx, cfg, inst)
	jsonDone := time.Now()
	res.CDPJSONAfterForkMs = ms(jsonDone.Sub(forkDone))
	if err != nil {
		res.TotalToBrowserReadyMs = msSince(totalStart)
		res.Error = fmt.Sprintf("cdp json: %v", err)
		return res
	}
	res.Browser = version.Browser
	res.ProtocolVersion = version.ProtocolVersion

	if err := browserGetVersion(ctx, cfg, versionURL, version.WebSocketDebuggerURL); err != nil {
		res.CDPGetVersionAfterMs = msSince(jsonDone)
		res.TotalToBrowserReadyMs = msSince(totalStart)
		res.Error = fmt.Sprintf("Browser.getVersion: %v", err)
		return res
	}
	res.CDPGetVersionAfterMs = msSince(jsonDone)
	res.TotalToBrowserReadyMs = msSince(totalStart)
	return res
}

func newAPIClient(cfg config) (*apiClient, error) {
	base, err := url.Parse(cfg.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("base URL must include scheme and host")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &apiClient{
		base: base,
		key:  cfg.apiKey,
		client: &http.Client{
			Timeout:   90 * time.Second,
			Transport: tr,
		},
	}, nil
}

func (c *apiClient) do(ctx context.Context, method string, path string, body any, out any) error {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w: %s", err, string(raw))
		}
	}
	return nil
}

func resolveSource(ctx context.Context, client *apiClient, cfg config) (*apiInstance, error) {
	if cfg.sourceInstance != "" {
		var inst apiInstance
		if err := client.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(cfg.sourceInstance), nil, &inst); err != nil {
			return nil, fmt.Errorf("get source instance: %w", err)
		}
		return &inst, nil
	}

	var instances []apiInstance
	if err := client.do(ctx, http.MethodGet, "/instances?state=Standby", nil, &instances); err != nil {
		return nil, fmt.Errorf("list standby instances: %w", err)
	}
	var nameRE *regexp.Regexp
	if cfg.sourceNameRegex != "" {
		re, err := regexp.Compile(cfg.sourceNameRegex)
		if err != nil {
			return nil, fmt.Errorf("compile source-name-regex: %w", err)
		}
		nameRE = re
	}
	candidates := make([]apiInstance, 0)
	for _, inst := range instances {
		if inst.State != "Standby" || !inst.HasSnapshot {
			continue
		}
		if !strings.Contains(inst.Image, "chromium-headful") {
			continue
		}
		lowerName := strings.ToLower(inst.Name)
		flags := strings.ToLower(inst.Env["CHROMIUM_FLAGS"])
		if strings.Contains(lowerName, "stealth") || strings.Contains(flags, "capmonster") || strings.Contains(flags, "--load-extension") {
			continue
		}
		if nameRE != nil && !nameRE.MatchString(inst.Name) {
			continue
		}
		candidates = append(candidates, inst)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})
	if len(candidates) == 0 {
		return nil, errors.New("no standby headful non-stealth browser source found; pass -source-instance")
	}
	return &candidates[0], nil
}

func ensureCDPIngress(ctx context.Context, client *apiClient, cfg config, runID string) (id string, created bool, err error) {
	var existing []apiIngress
	if err := client.do(ctx, http.MethodGet, "/ingresses", nil, &existing); err != nil {
		return "", false, fmt.Errorf("list ingresses: %w", err)
	}
	hostname := "{instance}." + cfg.cdpHostSuffix
	for _, ing := range existing {
		for _, rule := range ing.Rules {
			port := 80
			if rule.Match.Port != nil {
				port = *rule.Match.Port
			}
			tlsEnabled := rule.TLS != nil && *rule.TLS
			if rule.Match.Hostname == hostname &&
				port == cfg.cdpPublicPort &&
				rule.Target.Instance == "{instance}" &&
				rule.Target.Port == cfg.cdpTargetPort &&
				tlsEnabled == (cfg.cdpScheme == "https") {
				return ing.ID, false, nil
			}
		}
	}

	tlsEnabled := cfg.cdpScheme == "https"
	redirectHTTP := false
	body := map[string]any{
		"name": "bench-cdp-" + shortRunID(runID),
		"tags": map[string]string{
			"purpose": "browser-cold-start-bench",
			"run":     shortRunID(runID),
		},
		"rules": []any{
			map[string]any{
				"match": map[string]any{
					"hostname": hostname,
					"port":     cfg.cdpPublicPort,
				},
				"target": map[string]any{
					"instance": "{instance}",
					"port":     cfg.cdpTargetPort,
				},
				"tls":           tlsEnabled,
				"redirect_http": redirectHTTP,
			},
		},
	}
	var ing apiIngress
	if err := client.do(ctx, http.MethodPost, "/ingresses", body, &ing); err != nil {
		return "", false, fmt.Errorf("create cdp ingress: %w", err)
	}
	return ing.ID, true, nil
}

func waitForCDPVersion(ctx context.Context, cfg config, inst apiInstance) (*cdpVersion, *url.URL, error) {
	base, err := cdpVersionURL(cfg, inst)
	if err != nil {
		return nil, nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	httpClient := &http.Client{Timeout: 2 * time.Second, Transport: tr}
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				_ = resp.Body.Close()
				lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			} else {
				var version cdpVersion
				decodeErr := json.NewDecoder(resp.Body).Decode(&version)
				_ = resp.Body.Close()
				if decodeErr != nil {
					lastErr = decodeErr
				} else if version.WebSocketDebuggerURL == "" {
					lastErr = errors.New("missing webSocketDebuggerUrl")
				} else {
					return &version, base, nil
				}
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func cdpVersionURL(cfg config, inst apiInstance) (*url.URL, error) {
	host := inst.Name + "." + cfg.cdpHostSuffix
	port := cfg.cdpPublicPort
	if cfg.cdpDirectIP {
		if inst.Network == nil || inst.Network.IP == "" {
			return nil, errors.New("forked instance response has no network IP for direct CDP mode")
		}
		host = inst.Network.IP
		port = cfg.cdpTargetPort
	}
	return &url.URL{
		Scheme: cfg.cdpScheme,
		Host:   netHostPort(host, port, cfg.cdpScheme),
		Path:   "/json/version",
	}, nil
}

func browserGetVersion(ctx context.Context, cfg config, versionURL *url.URL, rawWSURL string) error {
	wsURL, err := normalizeWebSocketURL(rawWSURL, cfg, versionURL)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}
	if cfg.insecureTLS {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetReadDeadline(deadline)
		_ = conn.SetWriteDeadline(deadline)
	} else {
		deadline = time.Now().Add(10 * time.Second)
		_ = conn.SetReadDeadline(deadline)
		_ = conn.SetWriteDeadline(deadline)
	}
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Browser.getVersion"}); err != nil {
		return err
	}
	for {
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.ID != 1 {
			continue
		}
		if len(msg.Error) > 0 && string(msg.Error) != "null" {
			return fmt.Errorf("CDP error: %s", string(msg.Error))
		}
		var result struct {
			Product string `json:"product"`
		}
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return err
		}
		if result.Product == "" {
			return errors.New("Browser.getVersion result missing product")
		}
		return nil
	}
}

func normalizeWebSocketURL(raw string, cfg config, versionURL *url.URL) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		u.Scheme = "ws"
	}
	if cfg.cdpScheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	if versionURL != nil && versionURL.Host != "" {
		u.Host = versionURL.Host
	}
	if u.Host == "" {
		return "", fmt.Errorf("webSocketDebuggerUrl has no host: %s", raw)
	}
	return u.String(), nil
}

func netHostPort(host string, port int, scheme string) string {
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

func benchmarkName(prefix string, runID string, iter int) string {
	clean := sanitizeName(prefix)
	suffix := shortRunID(runID) + "-" + strconv.Itoa(iter)
	maxPrefix := 63 - len(suffix) - 1
	if maxPrefix < 1 {
		maxPrefix = 1
	}
	if len(clean) > maxPrefix {
		clean = strings.Trim(clean[:maxPrefix], "-")
	}
	if clean == "" {
		clean = "bench"
	}
	return clean + "-" + suffix
}

func sanitizeName(in string) string {
	in = strings.ToLower(in)
	var b strings.Builder
	lastDash := false
	for _, r := range in {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortRunID(runID string) string {
	if len(runID) <= 10 {
		return runID
	}
	return runID[len(runID)-10:]
}

func printSummary(results []iterationResult) {
	ok := make([]iterationResult, 0, len(results))
	for _, r := range results {
		if r.Error == "" {
			ok = append(ok, r)
		}
	}
	fmt.Printf("\nsummary: ok=%d failed=%d\n", len(ok), len(results)-len(ok))
	if len(ok) == 0 {
		printJSON(results)
		return
	}
	type series struct {
		Name string
		Vals []float64
	}
	sets := []series{
		{Name: "total_to_browser_ready_ms"},
		{Name: "fork_ms"},
		{Name: "cdp_json_after_fork_ms"},
		{Name: "cdp_get_version_after_json_ms"},
	}
	for _, r := range ok {
		sets[0].Vals = append(sets[0].Vals, r.TotalToBrowserReadyMs)
		sets[1].Vals = append(sets[1].Vals, r.ForkMs)
		sets[2].Vals = append(sets[2].Vals, r.CDPJSONAfterForkMs)
		sets[3].Vals = append(sets[3].Vals, r.CDPGetVersionAfterMs)
	}
	for _, set := range sets {
		stats := summarize(set.Vals)
		fmt.Printf("%-34s avg=%7.1f p50=%7.1f min=%7.1f max=%7.1f\n",
			set.Name, stats.Avg, stats.P50, stats.Min, stats.Max)
	}

	fmt.Println("\nresults_json:")
	printJSON(results)
}

type stats struct {
	Avg float64 `json:"avg"`
	P50 float64 `json:"p50"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func summarize(vals []float64) stats {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	return stats{
		Avg: sum / float64(len(sorted)),
		P50: percentile(sorted, 50),
		Min: sorted[0],
		Max: sorted[len(sorted)-1],
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := (p / 100) * float64(len(sorted)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return sorted[lower]
	}
	weight := pos - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func msSince(t time.Time) float64 {
	return ms(time.Since(t))
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func envString(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
