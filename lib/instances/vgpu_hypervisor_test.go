package instances

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func TestResolveCreateHypervisorForVGPU(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		request           CreateInstanceRequest
		defaultHypervisor hypervisor.Type
		want              hypervisor.Type
		wantErr           bool
	}{
		{
			name: "explicit qemu",
			request: CreateInstanceRequest{
				Hypervisor: hypervisor.TypeQEMU,
				GPU:        &GPUConfig{Profile: "NVIDIA L40S-2Q"},
			},
			defaultHypervisor: hypervisor.TypeCloudHypervisor,
			want:              hypervisor.TypeQEMU,
		},
		{
			name: "qemu default",
			request: CreateInstanceRequest{
				GPU: &GPUConfig{Profile: "NVIDIA L40S-2Q"},
			},
			defaultHypervisor: hypervisor.TypeQEMU,
			want:              hypervisor.TypeQEMU,
		},
		{
			name: "explicit cloud hypervisor",
			request: CreateInstanceRequest{
				Hypervisor: hypervisor.TypeCloudHypervisor,
				GPU:        &GPUConfig{Profile: "NVIDIA L40S-2Q"},
			},
			defaultHypervisor: hypervisor.TypeQEMU,
			wantErr:           true,
		},
		{
			name: "cloud hypervisor default",
			request: CreateInstanceRequest{
				GPU: &GPUConfig{Profile: "NVIDIA L40S-2Q"},
			},
			defaultHypervisor: hypervisor.TypeCloudHypervisor,
			wantErr:           true,
		},
		{
			name: "firecracker",
			request: CreateInstanceRequest{
				Hypervisor: hypervisor.TypeFirecracker,
				GPU:        &GPUConfig{Profile: "NVIDIA L40S-2Q"},
			},
			defaultHypervisor: hypervisor.TypeQEMU,
			wantErr:           true,
		},
		{
			name: "non-GPU cloud hypervisor",
			request: CreateInstanceRequest{
				Hypervisor: hypervisor.TypeCloudHypervisor,
			},
			defaultHypervisor: hypervisor.TypeQEMU,
			want:              hypervisor.TypeCloudHypervisor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveCreateHypervisor(tt.request, tt.defaultHypervisor)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("expected ErrInvalidRequest, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
