package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateRequestHealthCheck(t *testing.T) {
	t.Run("normalizes exec health check", func(t *testing.T) {
		req := &CreateInstanceRequest{
			Name:  "health-exec",
			Image: "docker.io/library/alpine:latest",
			HealthCheck: &healthcheck.Policy{
				Type: healthcheck.TypeExec,
				Exec: &healthcheck.ExecCheck{Command: []string{"true"}},
			},
		}

		err := validateCreateRequest(req)
		require.NoError(t, err)
		require.NotNil(t, req.HealthCheck)
		assert.Equal(t, "10s", req.HealthCheck.Interval)
		assert.Equal(t, "2s", req.HealthCheck.Timeout)
	})

	t.Run("rejects network health check without networking", func(t *testing.T) {
		req := &CreateInstanceRequest{
			Name:           "health-tcp",
			Image:          "docker.io/library/alpine:latest",
			NetworkEnabled: false,
			HealthCheck: &healthcheck.Policy{
				Type: healthcheck.TypeTCP,
				TCP:  &healthcheck.TCPCheck{Port: 8080},
			},
		}

		err := validateCreateRequest(req)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "network.enabled")
	})
}
