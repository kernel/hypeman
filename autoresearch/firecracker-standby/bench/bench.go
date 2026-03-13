package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
)

const (
	DefaultScenario       = "core"
	NetworkScenario       = "network"
	defaultImageRef       = "docker.io/library/alpine:latest"
	defaultInstanceName   = "autoresearch-firecracker-standby"
	defaultOperationWait  = 30 * time.Second
	defaultPrepareTimeout = 10 * time.Minute
)

type PrepareOptions struct {
	RepoRoot      string
	Scenario      string
	WorkspaceRoot string
}

type RunOptions struct {
	RepoRoot       string
	Scenario       string
	WorkspaceRoot  string
	Budget         time.Duration
	RunningTimeout time.Duration
	ResultsPath    string
	Status         string
	Description    string
}

type Manifest struct {
	RepoRoot      string    `json:"repo_root"`
	WorkspaceRoot string    `json:"workspace_root"`
	DataDir       string    `json:"data_dir"`
	Scenario      string    `json:"scenario"`
	ImageRef      string    `json:"image_ref"`
	InstanceID    string    `json:"instance_id"`
	InstanceName  string    `json:"instance_name"`
	Hypervisor    string    `json:"hypervisor"`
	Branch        string    `json:"branch,omitempty"`
	Commit        string    `json:"commit,omitempty"`
	PreparedAt    time.Time `json:"prepared_at"`
}

type CycleResult struct {
	Cycle            int       `json:"cycle"`
	StartedAt        time.Time `json:"started_at"`
	StandbyMs        float64   `json:"standby_ms"`
	RestoreAPIMs     float64   `json:"restore_api_ms"`
	RestoreRunningMs float64   `json:"restore_running_ms"`
	RestoreWaitingMs float64   `json:"restore_waiting_ms"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
}

type Summary struct {
	Scenario            string        `json:"scenario"`
	Budget              time.Duration `json:"budget"`
	Duration            time.Duration `json:"duration"`
	Cycles              int           `json:"cycles"`
	Failures            int           `json:"failures"`
	StandbyP50Ms        float64       `json:"standby_p50_ms"`
	RestoreAPIP50Ms     float64       `json:"restore_api_p50_ms"`
	RestoreRunningP50Ms float64       `json:"restore_running_p50_ms"`
	RestoreRunningP95Ms float64       `json:"restore_running_p95_ms"`
	ScoreMs             float64       `json:"score_ms"`
	Commit              string        `json:"commit,omitempty"`
	Branch              string        `json:"branch,omitempty"`
	ResultsPath         string        `json:"results_path,omitempty"`
	CycleResults        []CycleResult `json:"cycle_results"`
}

func Prepare(ctx context.Context, opts PrepareOptions) (*Manifest, error) {
	repoRoot, scenario, workspaceRoot, err := normalizeOptions(opts.RepoRoot, opts.Scenario, opts.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	dataDir := defaultDataDir(repoRoot, scenario)
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}

	// Keep historical benchmark outputs while resetting the prepared VM workspace.
	if err := os.RemoveAll(dataDir); err != nil {
		return nil, fmt.Errorf("reset workspace data dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace data dir: %w", err)
	}

	manifest, err := prepareWorkspace(ctx, repoRoot, workspaceRoot, dataDir, scenario)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(manifestPath(workspaceRoot), manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func Run(ctx context.Context, opts RunOptions) (*Summary, error) {
	repoRoot, scenario, workspaceRoot, err := normalizeOptions(opts.RepoRoot, opts.Scenario, opts.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	if opts.Budget <= 0 {
		return nil, fmt.Errorf("budget must be > 0")
	}
	if opts.RunningTimeout <= 0 {
		opts.RunningTimeout = defaultOperationWait
	}

	manifest, err := LoadManifest(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if manifest.Scenario != scenario {
		return nil, fmt.Errorf("manifest scenario mismatch: expected %s, got %s", scenario, manifest.Scenario)
	}

	h, err := newHarness(repoRoot, manifest.DataDir, scenario)
	if err != nil {
		return nil, err
	}

	inst, err := h.manager.GetInstance(ctx, manifest.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("load prepared instance: %w", err)
	}
	if inst.State != instances.StateRunning {
		if inst.State == instances.StateStandby {
			if _, err := h.manager.RestoreInstance(ctx, manifest.InstanceID); err != nil {
				return nil, fmt.Errorf("restore prepared instance before benchmark: %w", err)
			}
			if _, err := waitForInstanceState(ctx, h.manager, manifest.InstanceID, instances.StateRunning, opts.RunningTimeout); err != nil {
				return nil, fmt.Errorf("wait for prepared instance running before benchmark: %w", err)
			}
		} else {
			return nil, fmt.Errorf("prepared instance must start in running or standby state, got %s", inst.State)
		}
	}

	resultsPath := opts.ResultsPath
	if strings.TrimSpace(resultsPath) == "" {
		resultsPath = defaultResultsPath(workspaceRoot)
	}

	start := time.Now()
	deadline := start.Add(opts.Budget)
	results := make([]CycleResult, 0)
	failures := 0

	for time.Now().Before(deadline) {
		cycle := CycleResult{
			Cycle:     len(results) + 1,
			StartedAt: time.Now(),
			Status:    "ok",
		}

		standbyStart := time.Now()
		if _, err := h.manager.StandbyInstance(ctx, manifest.InstanceID); err != nil {
			cycle.Status = "fail"
			cycle.Error = fmt.Sprintf("standby failed: %v", err)
			cycle.StandbyMs = durationMs(time.Since(standbyStart))
			results = append(results, cycle)
			failures++
			break
		}
		cycle.StandbyMs = durationMs(time.Since(standbyStart))

		restoreStart := time.Now()
		if _, err := h.manager.RestoreInstance(ctx, manifest.InstanceID); err != nil {
			cycle.Status = "fail"
			cycle.Error = fmt.Sprintf("restore failed: %v", err)
			cycle.RestoreAPIMs = durationMs(time.Since(restoreStart))
			results = append(results, cycle)
			failures++
			break
		}
		restoreAPIDuration := time.Since(restoreStart)
		cycle.RestoreAPIMs = durationMs(restoreAPIDuration)

		if _, err := waitForInstanceState(ctx, h.manager, manifest.InstanceID, instances.StateRunning, opts.RunningTimeout); err != nil {
			cycle.Status = "fail"
			cycle.Error = fmt.Sprintf("wait for running failed: %v", err)
			cycle.RestoreRunningMs = durationMs(time.Since(restoreStart))
			cycle.RestoreWaitingMs = math.Max(0, cycle.RestoreRunningMs-cycle.RestoreAPIMs)
			results = append(results, cycle)
			failures++
			break
		}

		cycle.RestoreRunningMs = durationMs(time.Since(restoreStart))
		cycle.RestoreWaitingMs = math.Max(0, cycle.RestoreRunningMs-cycle.RestoreAPIMs)

		if err := validateScenarioState(ctx, scenario, h.manager, manifest.InstanceID); err != nil {
			cycle.Status = "fail"
			cycle.Error = fmt.Sprintf("post-restore validation failed: %v", err)
			results = append(results, cycle)
			failures++
			break
		}

		results = append(results, cycle)
	}

	summary := buildSummary(repoRoot, scenario, opts.Budget, time.Since(start), results, failures)
	summary.ResultsPath = resultsPath
	if err := writeJSON(lastRunPath(workspaceRoot), summary); err != nil {
		return nil, err
	}
	if err := appendResultsRow(resultsPath, summary, opts.Status, opts.Description); err != nil {
		return nil, err
	}

	if failures > 0 {
		return summary, fmt.Errorf("benchmark failed after %d cycles", len(results))
	}
	return summary, nil
}

func LoadManifest(workspaceRoot string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath(workspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &manifest, nil
}

func DefaultWorkspaceRoot(repoRoot string, scenario string) string {
	return filepath.Join(repoRoot, "tmp", "autoresearch-firecracker-standby", scenario)
}

func defaultDataDir(repoRoot string, scenario string) string {
	sum := crc32.ChecksumIEEE([]byte(filepath.Clean(repoRoot) + ":" + scenario))
	return filepath.Join(os.TempDir(), fmt.Sprintf("hypeman-fcsb-%08x", sum))
}

func defaultResultsPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, "results.tsv")
}

func manifestPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, "manifest.json")
}

func lastRunPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, "last-run.json")
}

type harness struct {
	manager      instances.Manager
	imageManager images.Manager
	system       system.Manager
	network      network.Manager
	workspace    string
	dataDir      string
}

func newHarness(repoRoot, dataDir, scenario string) (*harness, error) {
	cfg := &config.Config{
		DataDir: dataDir,
		Network: benchmarkNetworkConfig(dataDir, scenario),
	}
	p := paths.New(dataDir)

	imageManager, err := images.NewManager(p, 1, nil)
	if err != nil {
		return nil, fmt.Errorf("create image manager: %w", err)
	}

	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)

	limits := instances.ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  0,
		MaxMemoryPerInstance: 0,
	}
	manager := instances.NewManager(
		p,
		imageManager,
		systemManager,
		networkManager,
		deviceManager,
		volumeManager,
		limits,
		hypervisor.TypeFirecracker,
		nil,
		nil,
	)

	resourceManager := resources.NewManager(cfg, p)
	resourceManager.SetInstanceLister(manager)
	resourceManager.SetImageLister(imageManager)
	resourceManager.SetVolumeLister(volumeManager)
	if err := resourceManager.Initialize(context.Background()); err != nil {
		return nil, fmt.Errorf("initialize resource manager: %w", err)
	}
	manager.SetResourceValidator(resourceManager)

	return &harness{
		manager:      manager,
		imageManager: imageManager,
		system:       systemManager,
		network:      networkManager,
		workspace:    repoRoot,
		dataDir:      dataDir,
	}, nil
}

func prepareWorkspace(ctx context.Context, repoRoot, workspaceRoot, dataDir, scenario string) (*Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultPrepareTimeout)
	defer cancel()

	h, err := newHarness(repoRoot, dataDir, scenario)
	if err != nil {
		return nil, err
	}

	if err := h.system.EnsureSystemFiles(ctx); err != nil {
		return nil, fmt.Errorf("ensure system files: %w", err)
	}
	if scenario == NetworkScenario {
		if err := h.network.Initialize(ctx, nil); err != nil {
			return nil, fmt.Errorf("initialize benchmark network: %w", err)
		}
	}

	imageRef := benchmarkImageRef(defaultImageRef)
	if _, err := h.imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageRef}); err != nil {
		return nil, fmt.Errorf("warm benchmark image: %w", err)
	}
	if err := h.imageManager.WaitForReady(ctx, imageRef); err != nil {
		return nil, fmt.Errorf("wait for benchmark image ready: %w", err)
	}

	instanceName := fmt.Sprintf("%s-%s", defaultInstanceName, scenario)
	inst, err := h.manager.CreateInstance(ctx, instances.CreateInstanceRequest{
		Name:              instanceName,
		Image:             imageRef,
		Size:              1024 * 1024 * 1024,
		OverlaySize:       10 * 1024 * 1024 * 1024,
		Vcpus:             1,
		NetworkEnabled:    scenario == NetworkScenario,
		Hypervisor:        hypervisor.TypeFirecracker,
		Cmd:               []string{"sleep", "infinity"},
		SkipKernelHeaders: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create benchmark instance: %w", err)
	}

	if _, err := waitForInstanceState(ctx, h.manager, inst.Id, instances.StateRunning, defaultOperationWait); err != nil {
		return nil, fmt.Errorf("wait for benchmark instance running: %w", err)
	}

	branch, _ := gitOutput(repoRoot, "branch", "--show-current")
	commit, _ := gitOutput(repoRoot, "rev-parse", "--short", "HEAD")
	return &Manifest{
		RepoRoot:      repoRoot,
		WorkspaceRoot: workspaceRoot,
		DataDir:       dataDir,
		Scenario:      scenario,
		ImageRef:      imageRef,
		InstanceID:    inst.Id,
		InstanceName:  inst.Name,
		Hypervisor:    string(hypervisor.TypeFirecracker),
		Branch:        strings.TrimSpace(branch),
		Commit:        strings.TrimSpace(commit),
		PreparedAt:    time.Now().UTC(),
	}, nil
}

func benchmarkNetworkConfig(dataDir string, scenario string) config.NetworkConfig {
	if scenario == NetworkScenario {
		sum := crc32.ChecksumIEEE([]byte(filepath.Clean(dataDir)))
		octet := 10 + int(sum%200)
		return config.NetworkConfig{
			BridgeName: fmt.Sprintf("hb%06x", sum&0xffffff),
			SubnetCIDR: fmt.Sprintf("10.231.%d.0/24", octet),
			DNSServer:  "1.1.1.1",
		}
	}
	return config.NetworkConfig{
		BridgeName: "hypebench0",
		SubnetCIDR: "10.231.41.0/24",
		DNSServer:  "1.1.1.1",
	}
}

func validateScenarioState(ctx context.Context, scenario string, manager instances.Manager, instanceID string) error {
	inst, err := manager.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if scenario == NetworkScenario {
		if inst.IP == "" || inst.MAC == "" {
			return fmt.Errorf("network scenario requires non-empty IP and MAC after restore")
		}
	}
	return nil
}

func waitForInstanceState(ctx context.Context, manager instances.Manager, instanceID string, expected instances.State, timeout time.Duration) (*instances.Instance, error) {
	deadline := time.Now().Add(timeout)
	var lastState instances.State
	var lastErr error

	for time.Now().Before(deadline) {
		inst, err := manager.GetInstance(ctx, instanceID)
		if err == nil {
			lastState = inst.State
			if inst.State == expected {
				return inst, nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}

	if lastErr != nil {
		return nil, fmt.Errorf("instance %s did not reach %s within %s (last error: %w)", instanceID, expected, timeout, lastErr)
	}
	return nil, fmt.Errorf("instance %s did not reach %s within %s (last state: %s)", instanceID, expected, timeout, lastState)
}

func buildSummary(repoRoot, scenario string, budget, duration time.Duration, cycleResults []CycleResult, failures int) *Summary {
	standbyValues := make([]float64, 0, len(cycleResults))
	restoreAPIValues := make([]float64, 0, len(cycleResults))
	restoreRunningValues := make([]float64, 0, len(cycleResults))
	for _, cycle := range cycleResults {
		if cycle.Status != "ok" {
			continue
		}
		standbyValues = append(standbyValues, cycle.StandbyMs)
		restoreAPIValues = append(restoreAPIValues, cycle.RestoreAPIMs)
		restoreRunningValues = append(restoreRunningValues, cycle.RestoreRunningMs)
	}

	branch, _ := gitOutput(repoRoot, "branch", "--show-current")
	commit, _ := gitOutput(repoRoot, "rev-parse", "--short", "HEAD")

	summary := &Summary{
		Scenario:            scenario,
		Budget:              budget,
		Duration:            duration,
		Cycles:              len(cycleResults),
		Failures:            failures,
		StandbyP50Ms:        percentile(standbyValues, 50),
		RestoreAPIP50Ms:     percentile(restoreAPIValues, 50),
		RestoreRunningP50Ms: percentile(restoreRunningValues, 50),
		RestoreRunningP95Ms: percentile(restoreRunningValues, 95),
		Branch:              strings.TrimSpace(branch),
		Commit:              strings.TrimSpace(commit),
		CycleResults:        cycleResults,
	}
	summary.ScoreMs = summary.StandbyP50Ms + summary.RestoreRunningP50Ms
	return summary
}

func appendResultsRow(path string, summary *Summary, status string, description string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create results dir: %w", err)
	}

	needsHeader := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		needsHeader = true
	} else if err != nil {
		return fmt.Errorf("stat results file: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open results file: %w", err)
	}
	defer f.Close()

	if needsHeader {
		if _, err := fmt.Fprintln(f, "commit\tscore_ms\tstandby_p50_ms\trestore_running_p50_ms\trestore_running_p95_ms\tcycles\tstatus\tdescription"); err != nil {
			return fmt.Errorf("write results header: %w", err)
		}
	}

	finalStatus := strings.TrimSpace(status)
	if finalStatus == "" {
		if summary.Failures > 0 {
			finalStatus = "crash"
		} else {
			finalStatus = "candidate"
		}
	}

	if _, err := fmt.Fprintf(
		f,
		"%s\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%s\t%s\n",
		summary.Commit,
		summary.ScoreMs,
		summary.StandbyP50Ms,
		summary.RestoreRunningP50Ms,
		summary.RestoreRunningP95Ms,
		summary.Cycles,
		finalStatus,
		sanitizeTSVField(description),
	); err != nil {
		return fmt.Errorf("append results row: %w", err)
	}
	return nil
}

func normalizeOptions(repoRoot, scenario, workspaceRoot string) (string, string, string, error) {
	if strings.TrimSpace(repoRoot) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", "", fmt.Errorf("get working directory: %w", err)
		}
		repoRoot = cwd
	}
	repoRoot = filepath.Clean(repoRoot)

	scenario = strings.TrimSpace(scenario)
	if scenario == "" {
		scenario = DefaultScenario
	}
	if scenario != DefaultScenario && scenario != NetworkScenario {
		return "", "", "", fmt.Errorf("unsupported scenario %q", scenario)
	}

	if strings.TrimSpace(workspaceRoot) == "" {
		workspaceRoot = DefaultWorkspaceRoot(repoRoot, scenario)
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	return repoRoot, scenario, workspaceRoot, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write json %s: %w", path, err)
	}
	return nil
}

func benchmarkImageRef(source string) string {
	registry := strings.TrimSpace(os.Getenv("HYPEMAN_TEST_REGISTRY"))
	if registry == "" {
		return source
	}

	registry = strings.TrimPrefix(strings.TrimPrefix(registry, "http://"), "https://")
	if strings.HasPrefix(source, "docker.io/") {
		return registry + "/" + strings.TrimPrefix(source, "docker.io/")
	}
	return source
}

func gitOutput(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func sanitizeTSVField(value string) string {
	value = strings.ReplaceAll(value, "\t", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func percentile(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	position := (pct / 100) * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	frac := position - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

func durationMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}
