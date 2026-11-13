package builds

import "github.com/kernel/hypeman/lib/registrytoken"

// Registry tokens are minted by lib/registrytoken, which image publication uses
// without the rest of this package.
type (
	RepoPermission         = registrytoken.RepoPermission
	RegistryTokenClaims    = registrytoken.Claims
	RegistryTokenGenerator = registrytoken.Generator
)

var NewRegistryTokenGenerator = registrytoken.NewGenerator
