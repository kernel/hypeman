// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { buildHeaders } from '../internal/headers';
import { RequestOptions } from '../internal/request-options';
import { maybeMultipartFormRequestOptions } from '../internal/uploads';
import { path } from '../internal/utils/path';

export class Volumes extends APIResource {
  /**
   * Creates a new volume. Supports two modes:
   *
   * - JSON body: Creates an empty volume of the specified size
   * - Multipart form: Creates a volume pre-populated with content from a tar.gz
   *   archive
   *
   * @example
   * ```ts
   * const volume = await client.volumes.create({
   *   name: 'my-data-volume',
   *   size_gb: 10,
   * });
   * ```
   */
  create(body: VolumeCreateParams, options?: RequestOptions): APIPromise<Volume> {
    return this._client.post(
      '/volumes',
      maybeMultipartFormRequestOptions({ body, ...options }, this._client),
    );
  }

  /**
   * List volumes
   *
   * @example
   * ```ts
   * const volumes = await client.volumes.list();
   * ```
   */
  list(options?: RequestOptions): APIPromise<VolumeListResponse> {
    return this._client.get('/volumes', options);
  }

  /**
   * Delete volume
   *
   * @example
   * ```ts
   * await client.volumes.delete('id');
   * ```
   */
  delete(id: string, options?: RequestOptions): APIPromise<void> {
    return this._client.delete(path`/volumes/${id}`, {
      ...options,
      headers: buildHeaders([{ Accept: '*/*' }, options?.headers]),
    });
  }

  /**
   * Get volume details
   *
   * @example
   * ```ts
   * const volume = await client.volumes.get('id');
   * ```
   */
  get(id: string, options?: RequestOptions): APIPromise<Volume> {
    return this._client.get(path`/volumes/${id}`, options);
  }
}

export interface Volume {
  /**
   * Unique identifier
   */
  id: string;

  /**
   * Creation timestamp (RFC3339)
   */
  created_at: string;

  /**
   * Volume name
   */
  name: string;

  /**
   * Size in gigabytes
   */
  size_gb: number;

  /**
   * List of current attachments (empty if not attached)
   */
  attachments?: Array<VolumeAttachment>;
}

export interface VolumeAttachment {
  /**
   * ID of the instance this volume is attached to
   */
  instance_id: string;

  /**
   * Mount path in the guest
   */
  mount_path: string;

  /**
   * Whether the attachment is read-only
   */
  readonly: boolean;
}

export type VolumeListResponse = Array<Volume>;

export interface VolumeCreateParams {
  /**
   * Volume name
   */
  name: string;

  /**
   * Size in gigabytes
   */
  size_gb: number;

  /**
   * Optional custom identifier (auto-generated if not provided)
   */
  id?: string;
}

export declare namespace Volumes {
  export {
    type Volume as Volume,
    type VolumeAttachment as VolumeAttachment,
    type VolumeListResponse as VolumeListResponse,
    type VolumeCreateParams as VolumeCreateParams,
  };
}
