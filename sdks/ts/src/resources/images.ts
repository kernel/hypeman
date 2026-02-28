// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { buildHeaders } from '../internal/headers';
import { RequestOptions } from '../internal/request-options';
import { path } from '../internal/utils/path';

export class Images extends APIResource {
  /**
   * Pull and convert OCI image
   *
   * @example
   * ```ts
   * const image = await client.images.create({
   *   name: 'docker.io/library/nginx:latest',
   * });
   * ```
   */
  create(body: ImageCreateParams, options?: RequestOptions): APIPromise<Image> {
    return this._client.post('/images', { body, ...options });
  }

  /**
   * List images
   *
   * @example
   * ```ts
   * const images = await client.images.list();
   * ```
   */
  list(options?: RequestOptions): APIPromise<ImageListResponse> {
    return this._client.get('/images', options);
  }

  /**
   * Delete image
   *
   * @example
   * ```ts
   * await client.images.delete('name');
   * ```
   */
  delete(name: string, options?: RequestOptions): APIPromise<void> {
    return this._client.delete(path`/images/${name}`, {
      ...options,
      headers: buildHeaders([{ Accept: '*/*' }, options?.headers]),
    });
  }

  /**
   * Get image details
   *
   * @example
   * ```ts
   * const image = await client.images.get('name');
   * ```
   */
  get(name: string, options?: RequestOptions): APIPromise<Image> {
    return this._client.get(path`/images/${name}`, options);
  }
}

export interface Image {
  /**
   * Creation timestamp (RFC3339)
   */
  created_at: string;

  /**
   * Resolved manifest digest
   */
  digest: string;

  /**
   * Normalized OCI image reference (tag or digest)
   */
  name: string;

  /**
   * Build status
   */
  status: 'pending' | 'pulling' | 'converting' | 'ready' | 'failed';

  /**
   * CMD from container metadata
   */
  cmd?: Array<string> | null;

  /**
   * Entrypoint from container metadata
   */
  entrypoint?: Array<string> | null;

  /**
   * Environment variables from container metadata
   */
  env?: { [key: string]: string };

  /**
   * Error message if status is failed
   */
  error?: string | null;

  /**
   * Position in build queue (null if not queued)
   */
  queue_position?: number | null;

  /**
   * Disk size in bytes (null until ready)
   */
  size_bytes?: number | null;

  /**
   * Working directory from container metadata
   */
  working_dir?: string | null;
}

export type ImageListResponse = Array<Image>;

export interface ImageCreateParams {
  /**
   * OCI image reference (e.g., docker.io/library/nginx:latest)
   */
  name: string;
}

export declare namespace Images {
  export {
    type Image as Image,
    type ImageListResponse as ImageListResponse,
    type ImageCreateParams as ImageCreateParams,
  };
}
