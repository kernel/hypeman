// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { RequestOptions } from '../internal/request-options';

export class Resources extends APIResource {
  /**
   * Returns current host resource capacity, allocation status, and per-instance
   * breakdown. Resources include CPU, memory, disk, and network. Oversubscription
   * ratios are applied to calculate effective limits.
   */
  get(options?: RequestOptions): APIPromise<Resources> {
    return this._client.get('/resources', options);
  }
}

export interface DiskBreakdown {
  /**
   * Disk used by exported rootfs images
   */
  images_bytes?: number;

  /**
   * Disk used by OCI layer cache (shared blobs)
   */
  oci_cache_bytes?: number;

  /**
   * Disk used by instance overlays (rootfs + volume overlays)
   */
  overlays_bytes?: number;

  /**
   * Disk used by volumes
   */
  volumes_bytes?: number;
}

/**
 * Available vGPU profile
 */
export interface GPUProfile {
  /**
   * Number of instances that can be created with this profile
   */
  available: number;

  /**
   * Frame buffer size in MB
   */
  framebuffer_mb: number;

  /**
   * Profile name (user-facing)
   */
  name: string;
}

/**
 * GPU resource status. Null if no GPUs available.
 */
export interface GPUResourceStatus {
  /**
   * GPU mode (vgpu for SR-IOV/mdev, passthrough for whole GPU)
   */
  mode: 'vgpu' | 'passthrough';

  /**
   * Total slots (VFs for vGPU, physical GPUs for passthrough)
   */
  total_slots: number;

  /**
   * Slots currently in use
   */
  used_slots: number;

  /**
   * Physical GPUs (only in passthrough mode)
   */
  devices?: Array<PassthroughDevice>;

  /**
   * Available vGPU profiles (only in vGPU mode)
   */
  profiles?: Array<GPUProfile>;
}

/**
 * Physical GPU available for passthrough
 */
export interface PassthroughDevice {
  /**
   * Whether this GPU is available (not attached to an instance)
   */
  available: boolean;

  /**
   * GPU name
   */
  name: string;
}

export interface ResourceAllocation {
  /**
   * vCPUs allocated
   */
  cpu?: number;

  /**
   * Disk allocated in bytes (overlay + volumes)
   */
  disk_bytes?: number;

  /**
   * Disk I/O bandwidth limit in bytes/sec
   */
  disk_io_bps?: number;

  /**
   * Instance identifier
   */
  instance_id?: string;

  /**
   * Instance name
   */
  instance_name?: string;

  /**
   * Memory allocated in bytes
   */
  memory_bytes?: number;

  /**
   * Download bandwidth limit in bytes/sec (external→VM)
   */
  network_download_bps?: number;

  /**
   * Upload bandwidth limit in bytes/sec (VM→external)
   */
  network_upload_bps?: number;
}

export interface ResourceStatus {
  /**
   * Currently allocated resources
   */
  allocated: number;

  /**
   * Available for allocation (effective_limit - allocated)
   */
  available: number;

  /**
   * Raw host capacity
   */
  capacity: number;

  /**
   * Capacity after oversubscription (capacity \* ratio)
   */
  effective_limit: number;

  /**
   * Oversubscription ratio applied
   */
  oversub_ratio: number;

  /**
   * Resource type
   */
  type: string;

  /**
   * How capacity was determined (detected, configured)
   */
  source?: string;
}

export interface Resources {
  allocations: Array<ResourceAllocation>;

  cpu: ResourceStatus;

  disk: ResourceStatus;

  memory: ResourceStatus;

  network: ResourceStatus;

  disk_breakdown?: DiskBreakdown;

  disk_io?: ResourceStatus;

  /**
   * GPU resource status. Null if no GPUs available.
   */
  gpu?: GPUResourceStatus | null;
}

export declare namespace Resources {
  export {
    type DiskBreakdown as DiskBreakdown,
    type GPUProfile as GPUProfile,
    type GPUResourceStatus as GPUResourceStatus,
    type PassthroughDevice as PassthroughDevice,
    type ResourceAllocation as ResourceAllocation,
    type ResourceStatus as ResourceStatus,
    type Resources as Resources,
  };
}
