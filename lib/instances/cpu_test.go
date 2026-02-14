package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCalculateGuestTopology(t *testing.T) {
	// Host with 2 threads/core, 8 cores/socket, 2 sockets (common server config)
	host := &HostTopology{
		ThreadsPerCore: 2,
		CoresPerSocket: 8,
		Sockets:        2,
	}

	tests := []struct {
		name              string
		vcpus             int
		host              *HostTopology
		expectNil         bool
		expectedThreads   *int
		expectedCores     *int
		expectedDies      *int
		expectedPackages  *int
	}{
		{
			name:      "1 vCPU - use CH defaults",
			vcpus:     1,
			host:      host,
			expectNil: true,
		},
		{
			name:      "2 vCPUs - use CH defaults",
			vcpus:     2,
			host:      host,
			expectNil: true,
		},
		{
			name:             "4 vCPUs - 2 threads x 2 cores",
			vcpus:            4,
			host:             host,
			expectNil:        false,
			expectedThreads:  intPtr(2),
			expectedCores:    intPtr(2),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
		{
			name:             "8 vCPUs - 2 threads x 4 cores",
			vcpus:            8,
			host:             host,
			expectNil:        false,
			expectedThreads:  intPtr(2),
			expectedCores:    intPtr(4),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
		{
			name:             "16 vCPUs - 2 threads x 8 cores",
			vcpus:            16,
			host:             host,
			expectNil:        false,
			expectedThreads:  intPtr(2),
			expectedCores:    intPtr(8),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
		{
			name:             "32 vCPUs - 2 threads x 8 cores x 2 packages",
			vcpus:            32,
			host:             host,
			expectNil:        false,
			expectedThreads:  intPtr(2),
			expectedCores:    intPtr(8),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(2),
		},
		{
			name:      "nil host - return nil",
			vcpus:     4,
			host:      nil,
			expectNil: true,
		},
		{
			name:             "3 vCPUs odd number - 1 thread x 3 cores",
			vcpus:            3,
			host:             host,
			expectNil:        false,
			expectedThreads:  intPtr(1),
			expectedCores:    intPtr(3),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
		{
			name:             "6 vCPUs - 2 threads x 3 cores",
			vcpus:            6,
			host:             host,
			expectNil:        false,
			expectedThreads:  intPtr(2),
			expectedCores:    intPtr(3),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateGuestTopology(tt.vcpus, tt.host)

			if tt.expectNil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				if result != nil {
					assert.Equal(t, tt.expectedThreads, result.ThreadsPerCore)
					assert.Equal(t, tt.expectedCores, result.CoresPerDie)
					assert.Equal(t, tt.expectedDies, result.DiesPerPackage)
					assert.Equal(t, tt.expectedPackages, result.Packages)

					// Verify the topology multiplies to the expected vCPU count
					total := *result.ThreadsPerCore * *result.CoresPerDie * *result.DiesPerPackage * *result.Packages
					assert.Equal(t, tt.vcpus, total, "topology should multiply to vcpu count")
				}
			}
		})
	}
}

func TestCalculateGuestTopologyNoSMT(t *testing.T) {
	// Host without hyperthreading (1 thread/core)
	host := &HostTopology{
		ThreadsPerCore: 1,
		CoresPerSocket: 8,
		Sockets:        1,
	}

	tests := []struct {
		name             string
		vcpus            int
		expectedThreads  *int
		expectedCores    *int
		expectedDies     *int
		expectedPackages *int
	}{
		{
			name:             "4 vCPUs - 1 thread x 4 cores",
			vcpus:            4,
			expectedThreads:  intPtr(1),
			expectedCores:    intPtr(4),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
		{
			name:             "8 vCPUs - 1 thread x 8 cores",
			vcpus:            8,
			expectedThreads:  intPtr(1),
			expectedCores:    intPtr(8),
			expectedDies:     intPtr(1),
			expectedPackages: intPtr(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateGuestTopology(tt.vcpus, host)
			assert.NotNil(t, result)
			if result != nil {
				assert.Equal(t, tt.expectedThreads, result.ThreadsPerCore)
				assert.Equal(t, tt.expectedCores, result.CoresPerDie)
				assert.Equal(t, tt.expectedDies, result.DiesPerPackage)
				assert.Equal(t, tt.expectedPackages, result.Packages)
			}
		})
	}
}

func intPtr(i int) *int {
	return &i
}

