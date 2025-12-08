//go:build arm64

package ingress

import "embed"

//go:embed binaries/caddy/v2.10.2/aarch64/caddy
var caddyBinaryFS embed.FS

const caddyArch = "aarch64"
