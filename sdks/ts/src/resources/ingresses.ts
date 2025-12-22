// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { buildHeaders } from '../internal/headers';
import { RequestOptions } from '../internal/request-options';
import { path } from '../internal/utils/path';

export class Ingresses extends APIResource {
  /**
   * Create ingress
   *
   * @example
   * ```ts
   * const ingress = await client.ingresses.create({
   *   name: 'my-api-ingress',
   *   rules: [
   *     {
   *       match: { hostname: '{instance}.example.com' },
   *       target: { instance: '{instance}', port: 8080 },
   *     },
   *   ],
   * });
   * ```
   */
  create(body: IngressCreateParams, options?: RequestOptions): APIPromise<Ingress> {
    return this._client.post('/ingresses', { body, ...options });
  }

  /**
   * List ingresses
   *
   * @example
   * ```ts
   * const ingresses = await client.ingresses.list();
   * ```
   */
  list(options?: RequestOptions): APIPromise<IngressListResponse> {
    return this._client.get('/ingresses', options);
  }

  /**
   * Delete ingress
   *
   * @example
   * ```ts
   * await client.ingresses.delete('id');
   * ```
   */
  delete(id: string, options?: RequestOptions): APIPromise<void> {
    return this._client.delete(path`/ingresses/${id}`, {
      ...options,
      headers: buildHeaders([{ Accept: '*/*' }, options?.headers]),
    });
  }

  /**
   * Get ingress details
   *
   * @example
   * ```ts
   * const ingress = await client.ingresses.get('id');
   * ```
   */
  get(id: string, options?: RequestOptions): APIPromise<Ingress> {
    return this._client.get(path`/ingresses/${id}`, options);
  }
}

export interface Ingress {
  /**
   * Auto-generated unique identifier
   */
  id: string;

  /**
   * Creation timestamp (RFC3339)
   */
  created_at: string;

  /**
   * Human-readable name
   */
  name: string;

  /**
   * Routing rules for this ingress
   */
  rules: Array<IngressRule>;
}

export interface IngressMatch {
  /**
   * Hostname to match. Can be:
   *
   * - Literal: "api.example.com" (exact match on Host header)
   * - Pattern: "{instance}.example.com" (dynamic routing based on subdomain)
   *
   * Pattern hostnames use named captures in curly braces (e.g., {instance}, {app})
   * that extract parts of the hostname for routing. The extracted values can be
   * referenced in the target.instance field.
   */
  hostname: string;

  /**
   * Host port to listen on for this rule (default 80)
   */
  port?: number;
}

export interface IngressRule {
  match: IngressMatch;

  target: IngressTarget;

  /**
   * Auto-create HTTP to HTTPS redirect for this hostname (only applies when tls is
   * enabled)
   */
  redirect_http?: boolean;

  /**
   * Enable TLS termination (certificate auto-issued via ACME).
   */
  tls?: boolean;
}

export interface IngressTarget {
  /**
   * Target instance name, ID, or capture reference.
   *
   * - For literal hostnames: Use the instance name or ID directly (e.g., "my-api")
   * - For pattern hostnames: Reference a capture from the hostname (e.g.,
   *   "{instance}")
   *
   * When using pattern hostnames, the instance is resolved dynamically at request
   * time.
   */
  instance: string;

  /**
   * Target port on the instance
   */
  port: number;
}

export type IngressListResponse = Array<Ingress>;

export interface IngressCreateParams {
  /**
   * Human-readable name (lowercase letters, digits, and dashes only; cannot start or
   * end with a dash)
   */
  name: string;

  /**
   * Routing rules for this ingress
   */
  rules: Array<IngressRule>;
}

export declare namespace Ingresses {
  export {
    type Ingress as Ingress,
    type IngressMatch as IngressMatch,
    type IngressRule as IngressRule,
    type IngressTarget as IngressTarget,
    type IngressListResponse as IngressListResponse,
    type IngressCreateParams as IngressCreateParams,
  };
}
