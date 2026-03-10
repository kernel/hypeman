package instances

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListInstancesFilter_Matches(t *testing.T) {
	t.Parallel()
	running := StateRunning
	stopped := StateStopped

	inst := &Instance{
		StoredMetadata: StoredMetadata{
			Id:    "inst-1",
			Name:  "web-server",
			Image: "nginx:latest",
			Tags: map[string]string{
				"team": "backend",
				"env":  "staging",
			},
		},
		State: StateRunning,
	}

	tests := []struct {
		name   string
		filter *ListInstancesFilter
		want   bool
	}{
		{
			name:   "nil filter matches everything",
			filter: nil,
			want:   true,
		},
		{
			name:   "empty filter matches everything",
			filter: &ListInstancesFilter{},
			want:   true,
		},
		{
			name:   "state filter matches",
			filter: &ListInstancesFilter{State: &running},
			want:   true,
		},
		{
			name:   "state filter does not match",
			filter: &ListInstancesFilter{State: &stopped},
			want:   false,
		},
		{
			name: "single tag key matches",
			filter: &ListInstancesFilter{
				Tags: map[string]string{"team": "backend"},
			},
			want: true,
		},
		{
			name: "single tag key wrong value",
			filter: &ListInstancesFilter{
				Tags: map[string]string{"team": "frontend"},
			},
			want: false,
		},
		{
			name: "tag key does not exist",
			filter: &ListInstancesFilter{
				Tags: map[string]string{"project": "alpha"},
			},
			want: false,
		},
		{
			name: "multiple tag keys all match",
			filter: &ListInstancesFilter{
				Tags: map[string]string{
					"team": "backend",
					"env":  "staging",
				},
			},
			want: true,
		},
		{
			name: "multiple tag keys partial match",
			filter: &ListInstancesFilter{
				Tags: map[string]string{
					"team": "backend",
					"env":  "production",
				},
			},
			want: false,
		},
		{
			name: "state and tags combined match",
			filter: &ListInstancesFilter{
				State: &running,
				Tags:  map[string]string{"team": "backend"},
			},
			want: true,
		},
		{
			name: "state matches but tags do not",
			filter: &ListInstancesFilter{
				State: &running,
				Tags:  map[string]string{"team": "frontend"},
			},
			want: false,
		},
		{
			name: "tags match but state does not",
			filter: &ListInstancesFilter{
				State: &stopped,
				Tags:  map[string]string{"team": "backend"},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.filter.Matches(inst)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestListInstancesFilter_Matches_NilMetadata(t *testing.T) {
	t.Parallel()
	inst := &Instance{
		StoredMetadata: StoredMetadata{
			Id:   "inst-2",
			Tags: nil,
		},
		State: StateRunning,
	}

	filter := &ListInstancesFilter{
		Tags: map[string]string{"team": "backend"},
	}
	assert.False(t, filter.Matches(inst), "should not match when instance has no tags")
}

// TestListInstances_WithFilter exercises the full ListInstances path using
// on-disk metadata files (no KVM required).
func TestListInstances_WithFilter(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	mgr := &manager{paths: p}

	// Create three instances with different tags on disk
	instances := []StoredMetadata{
		{
			Id:             "inst-a",
			Name:           "web",
			Image:          "nginx:latest",
			Tags:           map[string]string{"team": "backend", "env": "prod"},
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
			SocketPath:     "/nonexistent/a.sock",
			DataDir:        p.InstanceDir("inst-a"),
		},
		{
			Id:             "inst-b",
			Name:           "worker",
			Image:          "python:3",
			Tags:           map[string]string{"team": "backend", "env": "staging"},
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
			SocketPath:     "/nonexistent/b.sock",
			DataDir:        p.InstanceDir("inst-b"),
		},
		{
			Id:             "inst-c",
			Name:           "frontend",
			Image:          "node:20",
			Tags:           map[string]string{"team": "frontend", "env": "prod"},
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeCloudHypervisor,
			SocketPath:     "/nonexistent/c.sock",
			DataDir:        p.InstanceDir("inst-c"),
		},
	}

	for _, stored := range instances {
		require.NoError(t, mgr.ensureDirectories(stored.Id))
		data, err := json.MarshalIndent(&metadata{StoredMetadata: stored}, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(p.InstanceDir(stored.Id), "metadata.json"), data, 0644))
	}

	ctx := context.Background()

	t.Run("no filter returns all", func(t *testing.T) {
		result, err := mgr.ListInstances(ctx, nil)
		require.NoError(t, err)
		assert.Len(t, result, 3)
	})

	t.Run("filter by single tag key", func(t *testing.T) {
		result, err := mgr.ListInstances(ctx, &ListInstancesFilter{
			Tags: map[string]string{"team": "backend"},
		})
		require.NoError(t, err)
		assert.Len(t, result, 2)
		names := []string{result[0].Name, result[1].Name}
		assert.ElementsMatch(t, []string{"web", "worker"}, names)
	})

	t.Run("filter by two tag keys", func(t *testing.T) {
		result, err := mgr.ListInstances(ctx, &ListInstancesFilter{
			Tags: map[string]string{"team": "backend", "env": "prod"},
		})
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, "web", result[0].Name)
	})

	t.Run("filter by tags with no matches", func(t *testing.T) {
		result, err := mgr.ListInstances(ctx, &ListInstancesFilter{
			Tags: map[string]string{"team": "devops"},
		})
		require.NoError(t, err)
		assert.Empty(t, result)
	})

	t.Run("filter by state", func(t *testing.T) {
		// All instances have no socket so they're Stopped
		stopped := StateStopped
		result, err := mgr.ListInstances(ctx, &ListInstancesFilter{
			State: &stopped,
		})
		require.NoError(t, err)
		assert.Len(t, result, 3)

		running := StateRunning
		result, err = mgr.ListInstances(ctx, &ListInstancesFilter{
			State: &running,
		})
		require.NoError(t, err)
		assert.Empty(t, result)
	})

	t.Run("filter by state and tags combined", func(t *testing.T) {
		stopped := StateStopped
		result, err := mgr.ListInstances(ctx, &ListInstancesFilter{
			State: &stopped,
			Tags:  map[string]string{"env": "prod"},
		})
		require.NoError(t, err)
		assert.Len(t, result, 2)
		names := []string{result[0].Name, result[1].Name}
		assert.ElementsMatch(t, []string{"web", "frontend"}, names)
	})
}
