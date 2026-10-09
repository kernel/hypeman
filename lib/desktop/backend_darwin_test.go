//go:build darwin

package desktop

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDarwinBrowserLaunchCommand(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret-not-to-inherit")
	b := &DarwinBackend{home: "/Users/test", uid: 501}
	cmd := b.chromeCommand()
	expected := []string{chromeBinary, "--user-data-dir=/Users/test/Library/Application Support/Hypeman/Chrome", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=9222", "--no-first-run", "--no-default-browser-check"}
	if !reflect.DeepEqual(cmd.Args, expected) {
		t.Fatal("unexpected Chrome command")
	}
	if !reflect.DeepEqual(cmd.Env, []string{"HOME=/Users/test", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}) {
		t.Fatal("unexpected browser environment")
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "headless") {
			t.Fatal("headless browser configured")
		}
	}
}

func TestDarwinBrowserListenerOwnership(t *testing.T) {
	if !ownsBrowserListener("p123\nf22\nn127.0.0.1:9222\n") {
		t.Fatal("owned listener rejected")
	}
	for _, output := range []string{"", "p123\nn*:9222\n", "p123\nn127.0.0.1:9223\n", "p123\nn127.0.0.1:9222evil\n"} {
		if ownsBrowserListener(output) {
			t.Fatal("unproven listener accepted")
		}
	}
}

func TestDarwinPrivateProfile(t *testing.T) {
	for _, kind := range []string{"private", "symlink", "shared"} {
		t.Run(kind, func(t *testing.T) {
			b := &DarwinBackend{uid: uint32(os.Getuid()), home: t.TempDir()}
			if err := b.prepareProfile(); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Remove(b.profilePath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), b.profilePath()); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "shared" {
				if err := os.Chmod(filepath.Dir(b.profilePath()), 0755); err != nil {
					t.Fatal(err)
				}
			}
			err := b.prepareProfile()
			if (err == nil) != (kind == "private") {
				t.Fatalf("profile policy: %v", err)
			}
		})
	}
}
