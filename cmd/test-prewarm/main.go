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
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
)

const (
	defaultRegistry = "127.0.0.1:5001"
	registryName    = "hypeman-ci-registry"
)

var defaultImages = []string{
	"docker.io/library/alpine:latest",
	"docker.io/library/nginx:alpine",
	"docker.io/bitnami/redis:latest",
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

	inspectClient, err := images.NewOCIClient(filepath.Join(prewarmDir, ".inspect-cache"))
	if err != nil {
		fatalf("create inspect client: %v", err)
	}

	manifest := prewarmManifest{
		WarmedAt:   time.Now().UTC().Format(time.RFC3339),
		Registry:   registry,
		PrewarmDir: prewarmDir,
		Images:     make([]manifestImage, 0, len(imagesToWarm)),
	}

	for _, source := range imagesToWarm {
		entry, err := ensureMirroredImage(ctx, inspectClient, registry, source)
		if err != nil {
			fatalf("prewarm image %s: %v", source, err)
		}
		fmt.Printf("prewarm image source=%s local=%s digest=%s cache_hit=%t\n", entry.Source, entry.LocalRef, entry.Digest, entry.CacheHit)
		manifest.Images = append(manifest.Images, entry)
	}

	p := paths.New(prewarmDir)
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

func ensureMirroredImage(ctx context.Context, inspector *images.OCIClient, registry, source string) (manifestImage, error) {
	localRef, err := toLocalRegistryRef(registry, source)
	if err != nil {
		return manifestImage{}, err
	}

	if digest, err := inspector.InspectManifest(ctx, localRef); err == nil {
		return manifestImage{Source: source, LocalRef: localRef, Digest: digest, CacheHit: true}, nil
	}

	res, err := images.MirrorBaseImage(ctx, "http://"+registry, images.MirrorRequest{SourceImage: source}, nil)
	if err != nil {
		return manifestImage{}, err
	}

	digest, err := inspector.InspectManifest(ctx, localRef)
	if err != nil {
		digest = res.Digest
	}
	return manifestImage{Source: source, LocalRef: localRef, Digest: digest, CacheHit: false}, nil
}

func toLocalRegistryRef(registry, source string) (string, error) {
	ref, err := images.ParseNormalizedRef(source)
	if err != nil {
		return "", fmt.Errorf("parse source ref %q: %w", source, err)
	}

	repo := strings.TrimPrefix(ref.Repository(), "docker.io/")
	if repo == ref.Repository() {
		repo = ref.Repository()
	}

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

func parseImageList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
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
