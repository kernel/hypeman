package hypervisor

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRegistryConcurrentRegisterAndEnumerate exercises the public registry
// under concurrent registration and enumeration. RegisterRuntime is public
// API, so custom backends may register while capability requests iterate the
// registry; without synchronization this crashes with "concurrent map
// iteration and map write" (and the race detector reports it). Registered
// types use unique test-only names so production registrations shared with
// unrelated parallel tests are never mutated.
func TestRegistryConcurrentRegisterAndEnumerate(t *testing.T) {
	t.Parallel()

	const writers = 8
	const iterations = 50

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range iterations {
				typ := Type(fmt.Sprintf("registry-concurrency-test-%d-%d", w, i))
				RegisterRuntime(typ, RuntimeRegistration{
					Capabilities: func() Capabilities { return Capabilities{SupportsVsock: true} },
					LaunchCheck:  func() error { return nil },
				})
				caps, ok := CapabilitiesForType(typ)
				require.True(t, ok)
				require.True(t, caps.SupportsVsock)
			}
		}()
		go func() {
			defer wg.Done()
			for range iterations {
				for _, rt := range RegisteredRuntimes() {
					_ = rt.Available()
				}
				_, _ = CapabilitiesForType(Type("registry-concurrency-test-missing"))
			}
		}()
	}
	wg.Wait()
}

// TestRegistryCallbacksRunOutsideLock pins that enumeration resolves
// registration callbacks after releasing the registry lock: a Capabilities
// resolver or LaunchCheck that re-enters the registry — including taking the
// write lock via RegisterRuntime — must not deadlock. If callbacks executed
// under even a read lock, the nested RegisterRuntime would block forever.
func TestRegistryCallbacksRunOutsideLock(t *testing.T) {
	t.Parallel()

	reentrant := Type("registry-reentrant-callback-test")
	nested := Type("registry-reentrant-callback-test-nested")
	RegisterRuntime(reentrant, RuntimeRegistration{
		Capabilities: func() Capabilities {
			RegisterRuntime(nested, RuntimeRegistration{
				Capabilities: func() Capabilities { return Capabilities{SupportsPause: true} },
			})
			return Capabilities{SupportsVsock: true}
		},
		LaunchCheck: func() error {
			_, _ = CapabilitiesForType(nested)
			return nil
		},
	})

	found := false
	for _, rt := range RegisteredRuntimes() {
		if rt.Type == reentrant {
			found = true
			require.True(t, rt.Capabilities.SupportsVsock)
			require.True(t, rt.Available())
		}
	}
	require.True(t, found, "re-entrant registration must not deadlock or drop the runtime")

	caps, ok := CapabilitiesForType(nested)
	require.True(t, ok, "registration performed inside a resolver must land in the registry")
	require.True(t, caps.SupportsPause)
}
