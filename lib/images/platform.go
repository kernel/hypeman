package images

import (
	"fmt"
	"runtime"
	"strings"

	gcr "github.com/google/go-containerregistry/pkg/v1"
)

// Platform identifies the OS/architecture variant of an image, modeled on
// Docker's --platform (os/arch[/variant]). Hypeman guests are always Linux, so
// OS is currently constrained to "linux", but the field exists for parity with
// Docker references and future non-Linux support.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

// ParsePlatform parses a Docker-style platform string of the form
// "os/arch[/variant]". A bare architecture ("amd64") defaults OS to "linux".
// The result is normalized (lowercased, common aliases mapped).
func ParsePlatform(s string) (Platform, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Platform{}, fmt.Errorf("%w: platform is empty", ErrInvalidPlatform)
	}

	// Accept 1-3 components, all non-empty: a bare arch ("amd64") is shorthand for
	// linux/<arch>, but "/amd64", "linux/", "linux/amd64/", and "a/b/c/d" are
	// malformed, not shorthand for a default.
	parts := strings.Split(s, "/")
	ok := len(parts) <= 3
	for _, c := range parts {
		if strings.TrimSpace(c) == "" {
			ok = false
		}
	}
	if !ok {
		return Platform{}, fmt.Errorf("%w %q: expected os/arch[/variant]", ErrInvalidPlatform, s)
	}

	var p Platform
	switch len(parts) {
	case 1:
		p = Platform{OS: "linux", Architecture: parts[0]}
	case 2:
		p = Platform{OS: parts[0], Architecture: parts[1]}
	case 3:
		p = Platform{OS: parts[0], Architecture: parts[1], Variant: parts[2]}
	}

	p = p.Normalize()
	if err := p.validate(); err != nil {
		return Platform{}, err
	}
	return p, nil
}

// Normalize lowercases the fields and maps common architecture aliases
// (x86_64 -> amd64, aarch64 -> arm64) so equality checks are reliable.
func (p Platform) Normalize() Platform {
	os := strings.ToLower(strings.TrimSpace(p.OS))
	if os == "" {
		os = "linux"
	}
	arch := strings.ToLower(strings.TrimSpace(p.Architecture))
	switch arch {
	case "x86_64", "x86-64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	return Platform{
		OS:           os,
		Architecture: arch,
		Variant:      strings.ToLower(strings.TrimSpace(p.Variant)),
	}
}

// validate enforces the platforms hypeman can actually boot today: Linux
// guests on amd64 or arm64. Other operating systems and architectures are
// rejected with an actionable error.
func (p Platform) validate() error {
	if p.OS != "linux" {
		return fmt.Errorf("%w: unsupported os %q: only linux guests are supported", ErrInvalidPlatform, p.OS)
	}
	switch p.Architecture {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("%w: unsupported architecture %q: must be amd64 or arm64", ErrInvalidPlatform, p.Architecture)
	}
}

// String renders the platform as "os/arch" (or "os/arch/variant" when a variant
// is set), matching Docker's canonical form.
func (p Platform) String() string {
	if p.OS == "" && p.Architecture == "" {
		return ""
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Matches reports whether two platforms identify the same guest OS and CPU
// architecture. Variant is intentionally ignored because Hypeman currently
// supports amd64 and arm64 only, where variant does not affect boot/emulation.
func (p Platform) Matches(other Platform) bool {
	p = p.Normalize()
	other = other.Normalize()
	return p.OS == other.OS && p.Architecture == other.Architecture
}

// ToGCR converts to a go-containerregistry platform for manifest selection.
func (p Platform) ToGCR() gcr.Platform {
	return gcr.Platform{
		OS:           p.OS,
		Architecture: p.Architecture,
		Variant:      p.Variant,
	}
}

// hostPlatform returns the platform of the host: a Linux guest on the host's
// architecture. Hypeman VMs always run Linux regardless of host OS.
func hostPlatform() Platform {
	return Platform{OS: "linux", Architecture: runtime.GOARCH}.Normalize()
}

// HostPlatformString renders the host platform as a canonical "os/arch" string.
// Exposed so the instances layer can resolve a no-platform create to the host
// variant explicitly rather than following a last-pull-wins tag pointer.
func HostPlatformString() string {
	return hostPlatform().String()
}

// needsEmulation reports whether running an image of platform img on a host of
// platform host requires CPU emulation (the architectures differ).
func needsEmulation(img, host Platform) bool {
	return img.Architecture != host.Architecture
}

// resolveRequestPlatform turns a (possibly empty) user-supplied platform string
// into a validated Platform. An empty string defaults to the host platform.
func resolveRequestPlatform(s string) (Platform, error) {
	if strings.TrimSpace(s) == "" {
		return hostPlatform(), nil
	}
	return ParsePlatform(s)
}

// resolveManifestPlatform returns the authoritative (normalized) platform to
// persist for a pulled image. The inline branches cover the three cases: an
// explicit request is matched against the manifest, a concrete manifest platform
// is validated, and an architecture-less manifest with no request falls back to
// the host (locally built/synthetic images often omit the platform).
func resolveManifestPlatform(meta *containerMetadata, requested string) (Platform, error) {
	actual := Platform{
		OS:           meta.OS,
		Architecture: meta.Architecture,
		Variant:      meta.Variant,
	}.Normalize()

	// An explicit request is authoritative for the match check and, when the
	// manifest omits its own platform, for the recorded value too.
	if strings.TrimSpace(requested) != "" {
		want, err := ParsePlatform(requested)
		if err != nil {
			return Platform{}, fmt.Errorf("requested platform: %w", err)
		}
		if actual.Architecture == "" {
			return want, nil
		}
		if err := actual.validate(); err != nil {
			return Platform{}, fmt.Errorf("image platform: %w", err)
		}
		if !want.Matches(actual) {
			return Platform{}, fmt.Errorf("%w: requested %s but manifest is %s", ErrInvalidPlatform, want, actual)
		}
		return actual, nil
	}

	// No request: a manifest with no declared architecture is assumed to be the
	// host platform, matching how pre-tracking images were treated.
	if actual.Architecture == "" {
		return hostPlatform(), nil
	}
	if err := actual.validate(); err != nil {
		return Platform{}, fmt.Errorf("image platform: %w", err)
	}
	return actual, nil
}

// validateDigestPlatform rejects a digest-pinned create whose explicit
// --platform conflicts with the manifest the digest actually points to. Only a
// user-supplied platform (requested != "") is authoritative: an empty request
// defaults to the host and must not reject a digest that simply targets another
// architecture. The error mirrors resolveManifestPlatform's "requested X but
// manifest is Y" wording for a consistent 400 across the create paths.
//
// requestedPlatform is the already-parsed/normalized form of requested; both are
// passed because resolveRequestPlatform collapses an empty request to the host
// platform, so the parsed value alone cannot distinguish "asked for host" from
// "asked for nothing".
func validateDigestPlatform(requested string, requestedPlatform, actual Platform) error {
	if strings.TrimSpace(requested) == "" {
		return nil
	}
	if !requestedPlatform.Matches(actual) {
		return fmt.Errorf("%w: requested %s but manifest is %s", ErrInvalidPlatform, requestedPlatform, actual)
	}
	return nil
}

// ImageNeedsHostEmulation reports whether an image whose stored platform string
// is platform requires CPU emulation to run on this host (its architecture
// differs from the host architecture). An empty platform is treated as the host
// platform. Exposed for the instances layer's boot-time emulation derivation.
func ImageNeedsHostEmulation(platform string) bool {
	host := hostPlatform()
	if strings.TrimSpace(platform) == "" {
		return false
	}
	p, err := ParsePlatform(platform)
	if err != nil {
		// Unparseable platforms are conservatively treated as host-native; the
		// build pipeline validates platforms, so this is defensive only.
		return false
	}
	return needsEmulation(p, host)
}
