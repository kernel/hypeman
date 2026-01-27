// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { Stream } from '../core/streaming';
import { type Uploadable } from '../core/uploads';
import { buildHeaders } from '../internal/headers';
import { RequestOptions } from '../internal/request-options';
import { multipartFormRequestOptions } from '../internal/uploads';
import { path } from '../internal/utils/path';

export class Builds extends APIResource {
  /**
   * Creates a new build job. Source code should be uploaded as a tar.gz archive in
   * the multipart form data.
   */
  create(body: BuildCreateParams, options?: RequestOptions): APIPromise<Build> {
    return this._client.post('/builds', multipartFormRequestOptions({ body, ...options }, this._client));
  }

  /**
   * List builds
   */
  list(options?: RequestOptions): APIPromise<BuildListResponse> {
    return this._client.get('/builds', options);
  }

  /**
   * Cancel build
   */
  cancel(id: string, options?: RequestOptions): APIPromise<void> {
    return this._client.delete(path`/builds/${id}`, {
      ...options,
      headers: buildHeaders([{ Accept: '*/*' }, options?.headers]),
    });
  }

  /**
   * Streams build events as Server-Sent Events. Events include:
   *
   * - `log`: Build log lines with timestamp and content
   * - `status`: Build status changes (queued→building→pushing→ready/failed)
   * - `heartbeat`: Keep-alive events sent every 30s to prevent connection timeouts
   *
   * Returns existing logs as events, then continues streaming if follow=true.
   */
  events(
    id: string,
    query: BuildEventsParams | undefined = {},
    options?: RequestOptions,
  ): APIPromise<Stream<BuildEvent>> {
    return this._client.get(path`/builds/${id}/events`, {
      query,
      ...options,
      headers: buildHeaders([{ Accept: 'text/event-stream' }, options?.headers]),
      stream: true,
    }) as APIPromise<Stream<BuildEvent>>;
  }

  /**
   * Get build details
   */
  get(id: string, options?: RequestOptions): APIPromise<Build> {
    return this._client.get(path`/builds/${id}`, options);
  }
}

export interface Build {
  /**
   * Build job identifier
   */
  id: string;

  /**
   * Build creation timestamp
   */
  created_at: string;

  /**
   * Build job status
   */
  status: BuildStatus;

  /**
   * Instance ID of the builder VM (for debugging)
   */
  builder_instance_id?: string | null;

  /**
   * Build completion timestamp
   */
  completed_at?: string | null;

  /**
   * Build duration in milliseconds
   */
  duration_ms?: number | null;

  /**
   * Error message (only when status is failed)
   */
  error?: string | null;

  /**
   * Digest of built image (only when status is ready)
   */
  image_digest?: string | null;

  /**
   * Full image reference (only when status is ready)
   */
  image_ref?: string | null;

  provenance?: BuildProvenance;

  /**
   * Position in build queue (only when status is queued)
   */
  queue_position?: number | null;

  /**
   * Build start timestamp
   */
  started_at?: string | null;
}

export interface BuildEvent {
  /**
   * Event timestamp
   */
  timestamp: string;

  /**
   * Event type
   */
  type: 'log' | 'status' | 'heartbeat';

  /**
   * Log line content (only for type=log)
   */
  content?: string;

  /**
   * New build status (only for type=status)
   */
  status?: BuildStatus;
}

export interface BuildPolicy {
  /**
   * Number of vCPUs for builder VM (default 2)
   */
  cpus?: number;

  /**
   * Memory limit for builder VM (default 2048)
   */
  memory_mb?: number;

  /**
   * Network access during build
   */
  network_mode?: 'isolated' | 'egress';

  /**
   * Maximum build duration (default 600)
   */
  timeout_seconds?: number;
}

export interface BuildProvenance {
  /**
   * Pinned base image digest used
   */
  base_image_digest?: string;

  /**
   * BuildKit version used
   */
  buildkit_version?: string;

  /**
   * Map of lockfile names to SHA256 hashes
   */
  lockfile_hashes?: { [key: string]: string };

  /**
   * SHA256 hash of source tarball
   */
  source_hash?: string;

  /**
   * Build completion timestamp
   */
  timestamp?: string;
}

/**
 * Build job status
 */
export type BuildStatus = 'queued' | 'building' | 'pushing' | 'ready' | 'failed' | 'cancelled';

export type BuildListResponse = Array<Build>;

export interface BuildCreateParams {
  /**
   * Source tarball (tar.gz) containing application code and optionally a Dockerfile
   */
  source: Uploadable;

  /**
   * Optional pinned base image digest
   */
  base_image_digest?: string;

  /**
   * Tenant-specific cache key prefix
   */
  cache_scope?: string;

  /**
   * Dockerfile content. Required if not included in the source tarball.
   */
  dockerfile?: string;

  /**
   * Global cache identifier (e.g., "node", "python", "ubuntu", "browser"). When
   * specified, the build will import from cache/global/{key}. Admin builds will also
   * export to this location.
   */
  global_cache_key?: string;

  /**
   * Set to "true" to grant push access to global cache (operator-only). Admin builds
   * can populate the shared global cache that all tenant builds read from.
   */
  is_admin_build?: string;

  /**
   * JSON array of secret references to inject during build. Each object has "id"
   * (required) for use with --mount=type=secret,id=... Example: [{"id":
   * "npm_token"}, {"id": "github_token"}]
   */
  secrets?: string;

  /**
   * Build timeout (default 600)
   */
  timeout_seconds?: number;
}

export interface BuildEventsParams {
  /**
   * Continue streaming new events after initial output
   */
  follow?: boolean;
}

export declare namespace Builds {
  export {
    type Build as Build,
    type BuildEvent as BuildEvent,
    type BuildPolicy as BuildPolicy,
    type BuildProvenance as BuildProvenance,
    type BuildStatus as BuildStatus,
    type BuildListResponse as BuildListResponse,
    type BuildCreateParams as BuildCreateParams,
    type BuildEventsParams as BuildEventsParams,
  };
}
