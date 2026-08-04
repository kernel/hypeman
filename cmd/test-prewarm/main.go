package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
)

const (
	defaultRegistry      = "127.0.0.1:5001"
	registryName         = "hypeman-ci-registry"
	initrdBuildRetention = 2 * time.Hour
)

// prewarmImage is a source image to mirror into the local registry. Platform
// is empty for host-platform images and set explicitly to mirror a specific
// variant (e.g. linux/amd64 for Rosetta emulation tests on an arm64 host).
type prewarmImage struct {
	Source   string
	Platform string
}

var defaultImages = []prewarmImage{
	{Source: "docker.io/library/alpine:latest"},
	{Source: "docker.io/library/alpine:3.18"},
	{Source: "docker.io/library/debian:12-slim"},
	{Source: "docker.io/library/nginx:alpine"},
	// Keep in sync with redisEntrypointEnvImage in lib/instances tests.
	{Source: "docker.io/bitnamilegacy/redis:7.2.5-debian-12-r0"},
	{Source: "docker.io/jrei/systemd-ubuntu:22.04"},
	// amd64-only mirror for the Rosetta x86 image E2E (single-platform manifest;
	// see toLocalRegistryRef for why it must be the only mirror of this tag).
	{Source: "docker.io/library/alpine:3.19", Platform: "linux/amd64"},
}

type manifestImage struct {
	Source   string `json:"source"`
	LocalRef string `json:"local_ref"`
	Digest   string `json:"digest"`
	CacheHit bool   `json:"cache_hit"`
}

type prewarmManifest struct {
	WarmedAt   string          `json:"warmed_at"`
	Registry   string          `json:"registry"`
	PrewarmDir string          `json:"prewarm_dir"`
	Images     []manifestImage `json:"images"`
	System     struct {
		KernelVersion string `json:"kernel_version"`
		Arch          string `json:"arch"`
		InitrdPath    string `json:"initrd_path"`
		InitrdHash    string `json:"initrd_hash"`
		CHBinaries    int    `json:"ch_binaries"`
		FCBinaryPath  string `json:"fc_binary_path"`
	} `json:"system"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	prewarmDir := envOr("HYPEMAN_TEST_PREWARM_DIR", defaultPrewarmDir())
	registry := trimRegistry(envOr("HYPEMAN_TEST_REGISTRY", defaultRegistry))
	imagesToWarm := parseImageList(os.Getenv("HYPEMAN_TEST_PREWARM_IMAGES"))
	if len(imagesToWarm) == 0 {
		imagesToWarm = defaultImages
	}

	if err := os.MkdirAll(prewarmDir, 0755); err != nil {
		fatalf("mkdir prewarm dir: %v", err)
	}

	unlock, err := lockFile(filepath.Join(prewarmDir, ".prewarm.lock"))
	if err != nil {
		fatalf("lock prewarm dir: %v", err)
	}
	defer unlock()

	if err := ensureLocalRegistry(ctx, registry, filepath.Join(prewarmDir, "registry")); err != nil {
		fatalf("ensure local registry: %v", err)
	}

	inspectClient, err := images.NewOCIClient(filepath.Join(prewarmDir, ".inspect-cache"), nil)
	if err != nil {
		fatalf("create inspect client: %v", err)
	}

	manifest := prewarmManifest{
		WarmedAt:   time.Now().UTC().Format(time.RFC3339),
		Registry:   registry,
		PrewarmDir: prewarmDir,
		Images:     make([]manifestImage, 0, len(imagesToWarm)),
	}
	p := paths.New(prewarmDir)
	readyImageManager, err := images.NewManager(p, 1, nil, nil)
	if err != nil {
		fatalf("create ready image manager: %v", err)
	}

	for _, img := range imagesToWarm {
		entry, err := ensureMirroredImage(ctx, inspectClient, registry, img)
		if err != nil {
			fatalf("prewarm image %s: %v", img.Source, err)
		}
		fmt.Printf("prewarm image source=%s local=%s digest=%s cache_hit=%t\n", entry.Source, entry.LocalRef, entry.Digest, entry.CacheHit)
		created, err := readyImageManager.CreateImage(ctx, images.CreateImageRequest{Name: entry.LocalRef, Platform: img.Platform})
		if err != nil {
			fatalf("build ready image %s: %v", entry.LocalRef, err)
		}
		waitName := created.Name
		if created.Digest != "" {
			ref, parseErr := images.ParseNormalizedRef(entry.LocalRef)
			if parseErr != nil {
				fatalf("parse ready image ref %s: %v", entry.LocalRef, parseErr)
			}
			waitName = ref.Repository() + "@" + created.Digest
		}
		if err := readyImageManager.WaitForReady(ctx, waitName); err != nil {
			fatalf("wait for ready image %s: %v", entry.LocalRef, err)
		}
		manifest.Images = append(manifest.Images, entry)
	}

	systemMgr := system.NewManager(p)
	if err := systemMgr.EnsureSystemFiles(ctx); err != nil {
		fatalf("prewarm system files: %v", err)
	}

	chBinaries, fcPath, err := ensureHypervisorBinaries(p)
	if err != nil {
		fatalf("prewarm hypervisor binaries: %v", err)
	}

	initrdPath, err := systemMgr.GetInitrdPath()
	if err != nil {
		fatalf("get initrd path: %v", err)
	}
	initrdHash, err := fileHash16(initrdPath)
	if err != nil {
		fatalf("hash initrd: %v", err)
	}
	pruned, err := pruneOldInitrdBuilds(initrdPath, time.Now().Add(-initrdBuildRetention))
	if err != nil {
		fatalf("prune old initrd builds: %v", err)
	}
	if pruned > 0 {
		fmt.Printf("pruned old initrd builds count=%d retention=%s\n", pruned, initrdBuildRetention)
	}

	manifest.System.KernelVersion = string(system.DefaultKernelVersion)
	manifest.System.Arch = system.GetArch()
	manifest.System.InitrdPath = initrdPath
	manifest.System.InitrdHash = initrdHash
	manifest.System.CHBinaries = chBinaries
	manifest.System.FCBinaryPath = fcPath

	manifestPath := filepath.Join(prewarmDir, "prewarm-manifest.json")
	if err := writeJSON(manifestPath, manifest); err != nil {
		fatalf("write manifest: %v", err)
	}
	fmt.Printf("prewarm complete manifest=%s\n", manifestPath)
}

func pruneOldInitrdBuilds(initrdPath string, cutoff time.Time) (int, error) {
	currentBuild := filepath.Base(filepath.Dir(initrdPath))
	initrdDir := filepath.Dir(filepath.Dir(initrdPath))
	latestTarget, err := os.Readlink(filepath.Join(initrdDir, "latest"))
	if err != nil {
		return 0, fmt.Errorf("read latest initrd: %w", err)
	}
	latestBuild := filepath.Base(filepath.Clean(latestTarget))

	entries, err := os.ReadDir(initrdDir)
	if err != nil {
		return 0, fmt.Errorf("read initrd builds: %w", err)
	}

	pruned := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == currentBuild || name == latestBuild {
			continue
		}
		if _, err := strconv.ParseInt(name, 10, 64); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return pruned, fmt.Errorf("stat initrd build %s: %w", name, err)
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(initrdDir, name)); err != nil {
			return pruned, fmt.Errorf("remove initrd build %s: %w", name, err)
		}
		pruned++
	}
	return pruned, nil
}

func ensureMirroredImage(ctx context.Context, inspector *images.OCIClient, registry string, img prewarmImage) (manifestImage, error) {
	localRef, err := toLocalRegistryRef(registry, img.Source)
	if err != nil {
		return manifestImage{}, err
	}

	inspectDigest := func() (string, error) {
		return inspector.InspectManifestForPlatform(ctx, localRef, img.Platform)
	}

	if digest, err := inspectDigest(); err == nil {
		return manifestImage{Source: img.Source, LocalRef: localRef, Digest: digest, CacheHit: true}, nil
	}

	res, err := images.MirrorBaseImage(ctx, "http://"+registry, images.MirrorRequest{
		SourceImage: img.Source,
		Platform:    img.Platform,
	}, nil, nil)
	if err != nil {
		return manifestImage{}, err
	}

	digest, err := inspectDigest()
	if err != nil {
		digest = res.Digest
	}
	return manifestImage{Source: img.Source, LocalRef: localRef, Digest: digest, CacheHit: false}, nil
}

// toLocalRegistryRef maps a source image to its local-registry reference.
// Platform-specific prewarm entries intentionally use the same ref as their
// source; defaultImages must not include another platform for the same source
// tag, so that local ref stays an unambiguous single-platform manifest.
func toLocalRegistryRef(registry, source string) (string, error) {
	ref, err := images.ParseNormalizedRef(source)
	if err != nil {
		return "", fmt.Errorf("parse source ref %q: %w", source, err)
	}

	repo := strings.TrimPrefix(ref.Repository(), "docker.io/")

	out := registry + "/" + repo
	if ref.Tag() != "" {
		return out + ":" + ref.Tag(), nil
	}
	if ref.Digest() != "" {
		return out + "@" + ref.Digest(), nil
	}
	return out + ":latest", nil
}

func ensureLocalRegistry(ctx context.Context, registry, dataDir string) error {
	if err := waitForRegistry(ctx, registry, 2*time.Second); err == nil {
		return nil
	}

	host, port, err := net.SplitHostPort(registry)
	if err != nil {
		return fmt.Errorf("registry must be host:port, got %q", registry)
	}
	if host != "127.0.0.1" && host != "localhost" {
		return fmt.Errorf("auto-start supports localhost registry only, got %q", registry)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	exists, err := dockerContainerExists(registryName)
	if err != nil {
		return err
	}

	if exists {
		if _, err := runCmd("docker", "start", registryName); err != nil {
			// Keep going; this may fail if already running.
			fmt.Printf("warning: docker start %s failed: %v\n", registryName, err)
		}
		if err := waitForRegistry(ctx, registry, 20*time.Second); err == nil {
			return nil
		}

		// Last resort for a broken existing container: recreate under lock.
		if _, err := runCmd("docker", "rm", "-f", registryName); err != nil {
			return err
		}
	}

	if _, err := runCmd("docker", "run", "-d", "--restart", "unless-stopped", "--name", registryName,
		"-p", fmt.Sprintf("%s:%s:5000", host, port),
		"-v", fmt.Sprintf("%s:/var/lib/registry", dataDir),
		"registry:2"); err != nil {
		return err
	}

	return waitForRegistry(ctx, registry, 20*time.Second)
}

func dockerContainerExists(name string) (bool, error) {
	out, err := runCmd("docker", "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

func waitForRegistry(ctx context.Context, registry string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := "http://" + registry + "/v2/"
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
					return nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("registry not healthy at %s", url)
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func fileHash16(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

func parseImageList(raw string) []prewarmImage {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]prewarmImage, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, prewarmImage{Source: p})
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func defaultPrewarmDir() string {
	osName := strings.ToLower(runtime.GOOS)
	arch := strings.ToLower(system.GetArch())
	cacheRoot, err := os.UserCacheDir()
	if err != nil || cacheRoot == "" {
		cacheRoot = filepath.Join(os.TempDir(), "cache")
	}
	return filepath.Join(cacheRoot, "hypeman-ci", osName+"-"+arch)
}

func trimRegistry(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "http://"), "https://")
	return strings.TrimSuffix(v, "/")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
