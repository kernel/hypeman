//go:build darwin

package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const chromeBinary = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// DarwinBackend runs in the selected user's Aqua LaunchAgent, never as root.
// It only controls a Chrome process it started; restart adoption is intentionally
// unsupported rather than attaching to an unrelated loopback debugging server.
type DarwinBackend struct {
	uid     uint32
	home    string
	mu      sync.Mutex
	process *exec.Cmd
	client  *http.Client
}

func NewDarwinBackend() (*DarwinBackend, error) {
	if os.Getuid() == 0 || os.Geteuid() != os.Getuid() || os.Getegid() != os.Getgid() || runtime.GOARCH != "arm64" {
		return nil, fmt.Errorf("desktop role requires a non-root Darwin/arm64 user")
	}
	account, err := user.Current()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(account.HomeDir) {
		return nil, fmt.Errorf("desktop user's home must be absolute")
	}
	return &DarwinBackend{uid: uint32(os.Getuid()), home: account.HomeDir, client: &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (b *DarwinBackend) Status(ctx context.Context) (Status, error) {
	s := Status{Version: ProtocolVersion, OS: "darwin", Architecture: runtime.GOARCH, UID: b.uid}
	console, err := os.Stat("/dev/console")
	if err != nil {
		return s, err
	}
	stat, ok := console.Sys().(*syscall.Stat_t)
	if !ok {
		return s, fmt.Errorf("console ownership unavailable")
	}
	s.ConsoleUID = stat.Uid
	gui, err := exec.CommandContext(ctx, "/bin/launchctl", "managername").Output()
	s.GUISession = err == nil && strings.TrimSpace(string(gui)) == "Aqua"
	b.mu.Lock()
	process := b.process
	b.mu.Unlock()
	if process == nil || !s.SessionReady() {
		return s, nil
	}
	// Starting a process and observing any open port is not proof of ownership.
	// lsof is restricted to our own launched PID and the fixed loopback listener.
	owners, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(process.Process.Pid), "-iTCP:9222", "-sTCP:LISTEN", "-Fn").Output()
	if err != nil || !ownsBrowserListener(string(owners)) {
		return s, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:9222/json/version", nil)
	if err != nil {
		return s, err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return s, nil
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxStatusBytes+1))
	if err != nil || len(data) > maxStatusBytes || response.StatusCode != 200 {
		return s, nil
	}
	var version struct {
		Browser  string `json:"Browser"`
		Debugger string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(data, &version) != nil || !strings.HasPrefix(version.Browser, "Chrome/") || len(version.Browser) > 256 {
		return s, nil
	}
	address, err := url.Parse(version.Debugger)
	if err != nil || address.Scheme != "ws" || address.Host != "127.0.0.1:9222" || address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || address.RawPath != "" || !strings.HasPrefix(address.Path, "/devtools/browser/") || !debuggerPath(address.Path) {
		return s, nil
	}
	b.mu.Lock()
	stillOwned := b.process == process
	b.mu.Unlock()
	if stillOwned {
		s.BrowserManaged = true
		s.BrowserReady = true
		s.Browser = version.Browser
	}
	return s, nil
}

func ownsBrowserListener(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if line == "n127.0.0.1:9222" {
			return true
		}
	}
	return false
}

func (b *DarwinBackend) profilePath() string {
	return filepath.Join(b.home, "Library", "Application Support", "Hypeman", "Chrome")
}

func (b *DarwinBackend) prepareProfile() error {
	// Refuse symlinks along the selected home-relative profile path. Existing
	// ordinary Library directories need not have private mode; our subtree must.
	current := b.home
	for _, part := range []string{"Library", "Application Support", "Hypeman", "Chrome"} {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || stat.Uid != b.uid {
			return fmt.Errorf("profile path is not a user-owned directory")
		}
		if (part == "Hypeman" || part == "Chrome") && info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("managed profile directory must be private")
		}
	}
	return nil
}

func (b *DarwinBackend) chromeCommand() *exec.Cmd {
	command := exec.Command(chromeBinary, "--user-data-dir="+b.profilePath(), "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=9222", "--no-first-run", "--no-default-browser-check")
	// Do not pass agent/service credentials through an inherited environment.
	command.Env = []string{"HOME=" + b.home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	return command
}

func (b *DarwinBackend) StartBrowser(ctx context.Context) (Status, error) {
	s, err := b.Status(ctx)
	if err != nil {
		return s, err
	}
	if !s.SessionReady() {
		return s, fmt.Errorf("selected Aqua session is not active")
	}
	if s.BrowserReady {
		return s, nil
	}
	b.mu.Lock()
	if b.process == nil {
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return s, err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:9222")
		if err != nil {
			b.mu.Unlock()
			return s, fmt.Errorf("refusing occupied unmanaged browser port")
		}
		_ = listener.Close()
		if err := b.prepareProfile(); err != nil {
			b.mu.Unlock()
			return s, err
		}
		command := b.chromeCommand()
		// The browser lifetime is not tied to this HTTP request, viewer or socket.
		if err := command.Start(); err != nil {
			b.mu.Unlock()
			return s, err
		}
		b.process = command
		go func() {
			_ = command.Wait()
			b.mu.Lock()
			if b.process == command {
				b.process = nil
			}
			b.mu.Unlock()
		}()
	}
	b.mu.Unlock()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err = b.Status(ctx)
		if err != nil || s.BrowserReady {
			return s, err
		}
		b.mu.Lock()
		exited := b.process == nil
		b.mu.Unlock()
		if exited {
			return s, fmt.Errorf("managed Chrome exited before readiness")
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-ticker.C:
		}
	}
}
