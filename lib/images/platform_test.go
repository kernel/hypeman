package images

import (
	"errors"
	"runtime"
	"testing"
)

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Platform
		wantErr bool
	}{
		{name: "bare arch defaults linux", in: "amd64", want: Platform{OS: "linux", Architecture: "amd64"}},
		{name: "os/arch", in: "linux/arm64", want: Platform{OS: "linux", Architecture: "arm64"}},
		{name: "os/arch/variant", in: "linux/arm/v7", wantErr: true}, // arm not in {amd64,arm64}
		{name: "x86_64 alias", in: "x86_64", want: Platform{OS: "linux", Architecture: "amd64"}},
		{name: "aarch64 alias", in: "linux/aarch64", want: Platform{OS: "linux", Architecture: "arm64"}},
		{name: "uppercase normalized", in: "LINUX/AMD64", want: Platform{OS: "linux", Architecture: "amd64"}},
		{name: "non-linux os rejected", in: "windows/amd64", wantErr: true},
		{name: "unknown arch rejected", in: "linux/riscv64", wantErr: true},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "too many parts", in: "a/b/c/d", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePlatform(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParsePlatform(%q) = %v, want error", tt.in, got)
				}
				if !errors.Is(err, ErrInvalidPlatform) {
					t.Fatalf("ParsePlatform(%q) error = %v, want ErrInvalidPlatform", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePlatform(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParsePlatform(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestPlatformString(t *testing.T) {
	if got := (Platform{OS: "linux", Architecture: "amd64"}).String(); got != "linux/amd64" {
		t.Fatalf("String() = %q, want linux/amd64", got)
	}
	if got := (Platform{OS: "linux", Architecture: "arm", Variant: "v7"}).String(); got != "linux/arm/v7" {
		t.Fatalf("String() = %q, want linux/arm/v7", got)
	}
	if got := (Platform{}).String(); got != "" {
		t.Fatalf("zero String() = %q, want empty", got)
	}
}

func TestNeedsEmulation(t *testing.T) {
	host := Platform{OS: "linux", Architecture: "arm64"}
	if !needsEmulation(Platform{OS: "linux", Architecture: "amd64"}, host) {
		t.Fatal("amd64 on arm64 host should need emulation")
	}
	if needsEmulation(Platform{OS: "linux", Architecture: "arm64"}, host) {
		t.Fatal("arm64 on arm64 host should not need emulation")
	}
}

func TestImageNeedsHostEmulation(t *testing.T) {
	hostArch := runtime.GOARCH
	other := "amd64"
	if hostArch == "amd64" {
		other = "arm64"
	}
	if ImageNeedsHostEmulation("") {
		t.Fatal("empty platform should be treated as host (no emulation)")
	}
	if ImageNeedsHostEmulation("linux/" + hostArch) {
		t.Fatalf("host platform linux/%s should not need emulation", hostArch)
	}
	if !ImageNeedsHostEmulation("linux/" + other) {
		t.Fatalf("non-host platform linux/%s should need emulation", other)
	}
	// Unparseable platform is treated conservatively as host-native.
	if ImageNeedsHostEmulation("not a platform") {
		t.Fatal("unparseable platform should not report emulation")
	}
}

func TestLocalPlatformTag(t *testing.T) {
	if got := LocalPlatformTag("3.19", ""); got != "3.19" {
		t.Fatalf("empty platform tag = %q, want 3.19", got)
	}
	if got := LocalPlatformTag("3.19", "linux/amd64"); got != "3.19-linux-amd64" {
		t.Fatalf("platform tag = %q, want 3.19-linux-amd64", got)
	}
	// Aliases normalize before encoding.
	if got := LocalPlatformTag("latest", "x86_64"); got != "latest-linux-amd64" {
		t.Fatalf("alias platform tag = %q, want latest-linux-amd64", got)
	}
}

func TestImageMetadataPlatformRoundTrip(t *testing.T) {
	// Stored platform survives the round trip to Image.
	meta := &imageMetadata{Name: "docker.io/library/alpine:3.19", Digest: "sha256:abc", Platform: "linux/amd64", Status: StatusReady}
	if got := meta.toImage().Platform; got != "linux/amd64" {
		t.Fatalf("toImage().Platform = %q, want linux/amd64", got)
	}

	// Empty stored platform defaults to the host platform (pre-tracking images).
	legacy := &imageMetadata{Name: "docker.io/library/alpine:latest", Digest: "sha256:def", Status: StatusReady}
	want := hostPlatform().String()
	if got := legacy.toImage().Platform; got != want {
		t.Fatalf("legacy toImage().Platform = %q, want host %q", got, want)
	}
}

func TestResolveManifestPlatform(t *testing.T) {
	amd64Meta := &containerMetadata{OS: "linux", Architecture: "amd64"}

	// Matching request (alias normalized) yields the authoritative platform.
	got, err := resolveManifestPlatform(amd64Meta, "x86_64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "linux/amd64" {
		t.Fatalf("got %s, want linux/amd64", got)
	}

	// No request still records the manifest platform.
	got, err = resolveManifestPlatform(amd64Meta, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "linux/amd64" {
		t.Fatalf("got %s, want linux/amd64", got)
	}

	// Mismatched request fails the build.
	if _, err := resolveManifestPlatform(amd64Meta, "linux/arm64"); err == nil || !errors.Is(err, ErrInvalidPlatform) {
		t.Fatalf("expected ErrInvalidPlatform for mismatch, got %v", err)
	}

	// An unsupported manifest os fails validation.
	if _, err := resolveManifestPlatform(&containerMetadata{OS: "windows", Architecture: "amd64"}, ""); err == nil {
		t.Fatal("expected error for non-linux manifest")
	}

	// A manifest with no declared architecture (locally built/synthetic image)
	// and no request falls back to the host platform.
	got, err = resolveManifestPlatform(&containerMetadata{}, "")
	if err != nil {
		t.Fatalf("unexpected error for platformless manifest: %v", err)
	}
	if got != hostPlatform() {
		t.Fatalf("platformless manifest = %s, want host %s", got, hostPlatform())
	}

	// A platformless manifest with an explicit request records the request.
	got, err = resolveManifestPlatform(&containerMetadata{}, "linux/amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "linux/amd64" {
		t.Fatalf("got %s, want linux/amd64", got)
	}
}

func TestResolveRequestPlatform(t *testing.T) {
	got, err := resolveRequestPlatform("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != hostPlatform() {
		t.Fatalf("empty request = %+v, want host %+v", got, hostPlatform())
	}
	if _, err := resolveRequestPlatform("linux/sparc"); err == nil {
		t.Fatal("expected error for unknown arch")
	}
}
