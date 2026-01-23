// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../../core/resource';
import * as VolumesAPI from './volumes';
import { VolumeAttachParams, VolumeDetachParams, Volumes } from './volumes';
import { APIPromise } from '../../core/api-promise';
import { Stream } from '../../core/streaming';
import { buildHeaders } from '../../internal/headers';
import { RequestOptions } from '../../internal/request-options';
import { path } from '../../internal/utils/path';

export class Instances extends APIResource {
  volumes: VolumesAPI.Volumes = new VolumesAPI.Volumes(this._client);

  /**
   * Create and start instance
   *
   * @example
   * ```ts
   * const instance = await client.instances.create({
   *   image: 'docker.io/library/alpine:latest',
   *   name: 'my-workload-1',
   * });
   * ```
   */
  create(body: InstanceCreateParams, options?: RequestOptions): APIPromise<Instance> {
    return this._client.post('/instances', { body, ...options });
  }

  /**
   * List instances
   *
   * @example
   * ```ts
   * const instances = await client.instances.list();
   * ```
   */
  list(options?: RequestOptions): APIPromise<InstanceListResponse> {
    return this._client.get('/instances', options);
  }

  /**
   * Stop and delete instance
   *
   * @example
   * ```ts
   * await client.instances.delete('id');
   * ```
   */
  delete(id: string, options?: RequestOptions): APIPromise<void> {
    return this._client.delete(path`/instances/${id}`, {
      ...options,
      headers: buildHeaders([{ Accept: '*/*' }, options?.headers]),
    });
  }

  /**
   * Get instance details
   *
   * @example
   * ```ts
   * const instance = await client.instances.get('id');
   * ```
   */
  get(id: string, options?: RequestOptions): APIPromise<Instance> {
    return this._client.get(path`/instances/${id}`, options);
  }

  /**
   * Streams instance logs as Server-Sent Events. Use the `source` parameter to
   * select which log to stream:
   *
   * - `app` (default): Guest application logs (serial console)
   * - `vmm`: Cloud Hypervisor VMM logs
   * - `hypeman`: Hypeman operations log
   *
   * Returns the last N lines (controlled by `tail` parameter), then optionally
   * continues streaming new lines if `follow=true`.
   *
   * @example
   * ```ts
   * const response = await client.instances.logs('id');
   * ```
   */
  logs(
    id: string,
    query: InstanceLogsParams | undefined = {},
    options?: RequestOptions,
  ): APIPromise<Stream<InstanceLogsResponse>> {
    return this._client.get(path`/instances/${id}/logs`, {
      query,
      ...options,
      headers: buildHeaders([{ Accept: 'text/event-stream' }, options?.headers]),
      stream: true,
    }) as APIPromise<Stream<InstanceLogsResponse>>;
  }

  /**
   * Restore instance from standby
   *
   * @example
   * ```ts
   * const instance = await client.instances.restore('id');
   * ```
   */
  restore(id: string, options?: RequestOptions): APIPromise<Instance> {
    return this._client.post(path`/instances/${id}/restore`, options);
  }

  /**
   * Put instance in standby (pause, snapshot, delete VMM)
   *
   * @example
   * ```ts
   * const instance = await client.instances.standby('id');
   * ```
   */
  standby(id: string, options?: RequestOptions): APIPromise<Instance> {
    return this._client.post(path`/instances/${id}/standby`, options);
  }

  /**
   * Start a stopped instance
   *
   * @example
   * ```ts
   * const instance = await client.instances.start('id');
   * ```
   */
  start(id: string, options?: RequestOptions): APIPromise<Instance> {
    return this._client.post(path`/instances/${id}/start`, options);
  }

  /**
   * Returns information about a path in the guest filesystem. Useful for checking if
   * a path exists, its type, and permissions before performing file operations.
   *
   * @example
   * ```ts
   * const pathInfo = await client.instances.stat('id', {
   *   path: 'path',
   * });
   * ```
   */
  stat(id: string, query: InstanceStatParams, options?: RequestOptions): APIPromise<PathInfo> {
    return this._client.get(path`/instances/${id}/stat`, { query, ...options });
  }

  /**
   * Stop instance (graceful shutdown)
   *
   * @example
   * ```ts
   * const instance = await client.instances.stop('id');
   * ```
   */
  stop(id: string, options?: RequestOptions): APIPromise<Instance> {
    return this._client.post(path`/instances/${id}/stop`, options);
  }
}

export interface Instance {
  /**
   * Auto-generated unique identifier (CUID2 format)
   */
  id: string;

  /**
   * Creation timestamp (RFC3339)
   */
  created_at: string;

  /**
   * OCI image reference
   */
  image: string;

  /**
   * Human-readable name
   */
  name: string;

  /**
   * Instance state:
   *
   * - Created: VMM created but not started (Cloud Hypervisor native)
   * - Running: VM is actively running (Cloud Hypervisor native)
   * - Paused: VM is paused (Cloud Hypervisor native)
   * - Shutdown: VM shut down but VMM exists (Cloud Hypervisor native)
   * - Stopped: No VMM running, no snapshot exists
   * - Standby: No VMM running, snapshot exists (can be restored)
   * - Unknown: Failed to determine state (see state_error for details)
   */
  state: 'Created' | 'Running' | 'Paused' | 'Shutdown' | 'Stopped' | 'Standby' | 'Unknown';

  /**
   * Disk I/O rate limit (human-readable, e.g., "100MB/s")
   */
  disk_io_bps?: string;

  /**
   * Environment variables
   */
  env?: { [key: string]: string };

  /**
   * GPU information attached to the instance
   */
  gpu?: Instance.GPU;

  /**
   * Whether a snapshot exists for this instance
   */
  has_snapshot?: boolean;

  /**
   * Hotplug memory size (human-readable)
   */
  hotplug_size?: string;

  /**
   * Hypervisor running this instance
   */
  hypervisor?: 'cloud-hypervisor' | 'qemu';

  /**
   * Network configuration of the instance
   */
  network?: Instance.Network;

  /**
   * Writable overlay disk size (human-readable)
   */
  overlay_size?: string;

  /**
   * Base memory size (human-readable)
   */
  size?: string;

  /**
   * Start timestamp (RFC3339)
   */
  started_at?: string | null;

  /**
   * Error message if state couldn't be determined (only set when state is Unknown)
   */
  state_error?: string | null;

  /**
   * Stop timestamp (RFC3339)
   */
  stopped_at?: string | null;

  /**
   * Number of virtual CPUs
   */
  vcpus?: number;

  /**
   * Volumes attached to the instance
   */
  volumes?: Array<VolumeMount>;
}

export namespace Instance {
  /**
   * GPU information attached to the instance
   */
  export interface GPU {
    /**
     * mdev device UUID
     */
    mdev_uuid?: string;

    /**
     * vGPU profile name
     */
    profile?: string;
  }

  /**
   * Network configuration of the instance
   */
  export interface Network {
    /**
     * Download bandwidth limit (human-readable, e.g., "1Gbps", "125MB/s")
     */
    bandwidth_download?: string;

    /**
     * Upload bandwidth limit (human-readable, e.g., "1Gbps", "125MB/s")
     */
    bandwidth_upload?: string;

    /**
     * Whether instance is attached to the default network
     */
    enabled?: boolean;

    /**
     * Assigned IP address (null if no network)
     */
    ip?: string | null;

    /**
     * Assigned MAC address (null if no network)
     */
    mac?: string | null;

    /**
     * Network name (always "default" when enabled)
     */
    name?: string;
  }
}

export interface PathInfo {
  /**
   * Whether the path exists
   */
  exists: boolean;

  /**
   * Error message if stat failed (e.g., permission denied). Only set when exists is
   * false due to an error rather than the path not existing.
   */
  error?: string | null;

  /**
   * True if this is a directory
   */
  is_dir?: boolean;

  /**
   * True if this is a regular file
   */
  is_file?: boolean;

  /**
   * True if this is a symbolic link (only set when follow_links=false)
   */
  is_symlink?: boolean;

  /**
   * Symlink target path (only set when is_symlink=true)
   */
  link_target?: string | null;

  /**
   * File mode (Unix permissions)
   */
  mode?: number;

  /**
   * File size in bytes
   */
  size?: number;
}

export interface PortMapping {
  /**
   * Port in the guest VM
   */
  guest_port: number;

  /**
   * Port on the host
   */
  host_port: number;

  protocol?: 'tcp' | 'udp';
}

export interface VolumeMount {
  /**
   * Path where volume is mounted in the guest
   */
  mount_path: string;

  /**
   * Volume identifier
   */
  volume_id: string;

  /**
   * Create per-instance overlay for writes (requires readonly=true)
   */
  overlay?: boolean;

  /**
   * Max overlay size as human-readable string (e.g., "1GB"). Required if
   * overlay=true.
   */
  overlay_size?: string;

  /**
   * Whether volume is mounted read-only
   */
  readonly?: boolean;
}

export type InstanceListResponse = Array<Instance>;

export type InstanceLogsResponse = string;

export interface InstanceCreateParams {
  /**
   * OCI image reference
   */
  image: string;

  /**
   * Human-readable name (lowercase letters, digits, and dashes only; cannot start or
   * end with a dash)
   */
  name: string;

  /**
   * Device IDs or names to attach for GPU/PCI passthrough
   */
  devices?: Array<string>;

  /**
   * Disk I/O rate limit (e.g., "100MB/s", "500MB/s"). Defaults to proportional share
   * based on CPU allocation if configured.
   */
  disk_io_bps?: string;

  /**
   * Environment variables
   */
  env?: { [key: string]: string };

  /**
   * GPU configuration for the instance
   */
  gpu?: InstanceCreateParams.GPU;

  /**
   * Additional memory for hotplug (human-readable format like "3GB", "1G")
   */
  hotplug_size?: string;

  /**
   * Hypervisor to use for this instance. Defaults to server configuration.
   */
  hypervisor?: 'cloud-hypervisor' | 'qemu';

  /**
   * Network configuration for the instance
   */
  network?: InstanceCreateParams.Network;

  /**
   * Writable overlay disk size (human-readable format like "10GB", "50G")
   */
  overlay_size?: string;

  /**
   * Base memory size (human-readable format like "1GB", "512MB", "2G")
   */
  size?: string;

  /**
   * Skip guest-agent installation during boot. When true, the exec and stat APIs
   * will not work for this instance. The instance will still run, but remote command
   * execution will be unavailable.
   */
  skip_guest_agent?: boolean;

  /**
   * Skip kernel headers installation during boot for faster startup. When true, DKMS
   * (Dynamic Kernel Module Support) will not work, preventing compilation of
   * out-of-tree kernel modules (e.g., NVIDIA vGPU drivers). Recommended for
   * workloads that don't need kernel module compilation.
   */
  skip_kernel_headers?: boolean;

  /**
   * Number of virtual CPUs
   */
  vcpus?: number;

  /**
   * Volumes to attach to the instance at creation time
   */
  volumes?: Array<VolumeMount>;
}

export namespace InstanceCreateParams {
  /**
   * GPU configuration for the instance
   */
  export interface GPU {
    /**
     * vGPU profile name (e.g., "L40S-1Q"). Only used in vGPU mode.
     */
    profile?: string;
  }

  /**
   * Network configuration for the instance
   */
  export interface Network {
    /**
     * Download bandwidth limit (external→VM, e.g., "1Gbps", "125MB/s"). Defaults to
     * proportional share based on CPU allocation.
     */
    bandwidth_download?: string;

    /**
     * Upload bandwidth limit (VM→external, e.g., "1Gbps", "125MB/s"). Defaults to
     * proportional share based on CPU allocation.
     */
    bandwidth_upload?: string;

    /**
     * Whether to attach instance to the default network
     */
    enabled?: boolean;
  }
}

export interface InstanceLogsParams {
  /**
   * Continue streaming new lines after initial output
   */
  follow?: boolean;

  /**
   * Log source to stream:
   *
   * - app: Guest application logs (serial console output)
   * - vmm: Cloud Hypervisor VMM logs (hypervisor stdout+stderr)
   * - hypeman: Hypeman operations log (actions taken on this instance)
   */
  source?: 'app' | 'vmm' | 'hypeman';

  /**
   * Number of lines to return from end
   */
  tail?: number;
}

export interface InstanceStatParams {
  /**
   * Path to stat in the guest filesystem
   */
  path: string;

  /**
   * Follow symbolic links (like stat vs lstat)
   */
  follow_links?: boolean;
}

Instances.Volumes = Volumes;

export declare namespace Instances {
  export {
    type Instance as Instance,
    type PathInfo as PathInfo,
    type PortMapping as PortMapping,
    type VolumeMount as VolumeMount,
    type InstanceListResponse as InstanceListResponse,
    type InstanceLogsResponse as InstanceLogsResponse,
    type InstanceCreateParams as InstanceCreateParams,
    type InstanceLogsParams as InstanceLogsParams,
    type InstanceStatParams as InstanceStatParams,
  };

  export {
    Volumes as Volumes,
    type VolumeAttachParams as VolumeAttachParams,
    type VolumeDetachParams as VolumeDetachParams,
  };
}
