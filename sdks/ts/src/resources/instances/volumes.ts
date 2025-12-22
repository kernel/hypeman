// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../../core/resource';
import * as InstancesAPI from './instances';
import { APIPromise } from '../../core/api-promise';
import { RequestOptions } from '../../internal/request-options';
import { path } from '../../internal/utils/path';

export class Volumes extends APIResource {
  /**
   * Attach volume to instance
   *
   * @example
   * ```ts
   * const instance = await client.instances.volumes.attach(
   *   'volumeId',
   *   { id: 'id', mount_path: '/mnt/data' },
   * );
   * ```
   */
  attach(
    volumeID: string,
    params: VolumeAttachParams,
    options?: RequestOptions,
  ): APIPromise<InstancesAPI.Instance> {
    const { id, ...body } = params;
    return this._client.post(path`/instances/${id}/volumes/${volumeID}`, { body, ...options });
  }

  /**
   * Detach volume from instance
   *
   * @example
   * ```ts
   * const instance = await client.instances.volumes.detach(
   *   'volumeId',
   *   { id: 'id' },
   * );
   * ```
   */
  detach(
    volumeID: string,
    params: VolumeDetachParams,
    options?: RequestOptions,
  ): APIPromise<InstancesAPI.Instance> {
    const { id } = params;
    return this._client.delete(path`/instances/${id}/volumes/${volumeID}`, options);
  }
}

export interface VolumeAttachParams {
  /**
   * Path param: Instance ID or name
   */
  id: string;

  /**
   * Body param: Path where volume should be mounted
   */
  mount_path: string;

  /**
   * Body param: Mount as read-only
   */
  readonly?: boolean;
}

export interface VolumeDetachParams {
  /**
   * Instance ID or name
   */
  id: string;
}

export declare namespace Volumes {
  export { type VolumeAttachParams as VolumeAttachParams, type VolumeDetachParams as VolumeDetachParams };
}
