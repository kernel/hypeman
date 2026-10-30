package builds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachinePolicyNormalization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy BuildPolicy
		err    string
	}{
		{name: "defaults"},
		{name: "minimum timeout", policy: BuildPolicy{TimeoutSeconds: 1}},
		{name: "maximum timeout", policy: BuildPolicy{TimeoutSeconds: 86400}},
		{name: "egress", policy: BuildPolicy{NetworkMode: "egress"}},
		{name: "negative timeout", policy: BuildPolicy{TimeoutSeconds: -1}, err: "invalid machine build timeout"},
		{name: "excessive timeout", policy: BuildPolicy{TimeoutSeconds: 86401}, err: "invalid machine build timeout"},
		{name: "unsupported mode", policy: BuildPolicy{NetworkMode: "host"}, err: "unsupported machine network policy"},
		{name: "domains", policy: BuildPolicy{NetworkMode: "egress", AllowedDomains: []string{"example.test"}}, err: "unsupported machine network policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy
			err := normalizeMachinePolicy(&policy)
			if tc.err != "" {
				require.EqualError(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			want := tc.policy
			if want.TimeoutSeconds == 0 {
				want.TimeoutSeconds = 600
			}
			if want.NetworkMode == "" {
				want.NetworkMode = "isolated"
			}
			require.Equal(t, want, policy)
			require.NoError(t, normalizeMachinePolicy(&policy))
			require.Equal(t, want, policy)
		})
	}
}
