// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { RequestOptions } from '../internal/request-options';

export class Health extends APIResource {
  /**
   * Health check
   */
  check(options?: RequestOptions): APIPromise<HealthCheckResponse> {
    return this._client.get('/health', options);
  }
}

export interface HealthCheckResponse {
  status: 'ok';
}

export declare namespace Health {
  export { type HealthCheckResponse as HealthCheckResponse };
}
