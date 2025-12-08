//go:build amd64

package ingress

import "embed"

//go:embed binaries/caddy/v2.10.2/x86_64/caddy
var caddyBinaryFS embed.FS

const caddyArch = "x86_64"
