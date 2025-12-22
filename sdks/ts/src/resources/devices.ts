// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

import { APIResource } from '../core/resource';
import { APIPromise } from '../core/api-promise';
import { buildHeaders } from '../internal/headers';
import { RequestOptions } from '../internal/request-options';
import { path } from '../internal/utils/path';

export class Devices extends APIResource {
  /**
   * Register a device for passthrough
   *
   * @example
   * ```ts
   * const device = await client.devices.create({
   *   pci_address: '0000:a2:00.0',
   * });
   * ```
   */
  create(body: DeviceCreateParams, options?: RequestOptions): APIPromise<Device> {
    return this._client.post('/devices', { body, ...options });
  }

  /**
   * Get device details
   *
   * @example
   * ```ts
   * const device = await client.devices.retrieve('id');
   * ```
   */
  retrieve(id: string, options?: RequestOptions): APIPromise<Device> {
    return this._client.get(path`/devices/${id}`, options);
  }

  /**
   * List registered devices
   *
   * @example
   * ```ts
   * const devices = await client.devices.list();
   * ```
   */
  list(options?: RequestOptions): APIPromise<DeviceListResponse> {
    return this._client.get('/devices', options);
  }

  /**
   * Unregister device
   *
   * @example
   * ```ts
   * await client.devices.delete('id');
   * ```
   */
  delete(id: string, options?: RequestOptions): APIPromise<void> {
    return this._client.delete(path`/devices/${id}`, {
      ...options,
      headers: buildHeaders([{ Accept: '*/*' }, options?.headers]),
    });
  }

  /**
   * Discover passthrough-capable devices on host
   *
   * @example
   * ```ts
   * const availableDevices =
   *   await client.devices.listAvailable();
   * ```
   */
  listAvailable(options?: RequestOptions): APIPromise<DeviceListAvailableResponse> {
    return this._client.get('/devices/available', options);
  }
}

export interface AvailableDevice {
  /**
   * PCI device ID (hex)
   */
  device_id: string;

  /**
   * IOMMU group number
   */
  iommu_group: number;

  /**
   * PCI address
   */
  pci_address: string;

  /**
   * PCI vendor ID (hex)
   */
  vendor_id: string;

  /**
   * Currently bound driver (null if none)
   */
  current_driver?: string | null;

  /**
   * Human-readable device name
   */
  device_name?: string;

  /**
   * Human-readable vendor name
   */
  vendor_name?: string;
}

export interface Device {
  /**
   * Auto-generated unique identifier (CUID2 format)
   */
  id: string;

  /**
   * Whether the device is currently bound to the vfio-pci driver, which is required
   * for VM passthrough.
   *
   * - true: Device is bound to vfio-pci and ready for (or currently in use by) a VM.
   *   The device's native driver has been unloaded.
   * - false: Device is using its native driver (e.g., nvidia) or no driver. Hypeman
   *   will automatically bind to vfio-pci when attaching to an instance.
   */
  bound_to_vfio: boolean;

  /**
   * Registration timestamp (RFC3339)
   */
  created_at: string;

  /**
   * PCI device ID (hex)
   */
  device_id: string;

  /**
   * IOMMU group number
   */
  iommu_group: number;

  /**
   * PCI address
   */
  pci_address: string;

  /**
   * Type of PCI device
   */
  type: DeviceType;

  /**
   * PCI vendor ID (hex)
   */
  vendor_id: string;

  /**
   * Instance ID if attached
   */
  attached_to?: string | null;

  /**
   * Device name (user-provided or auto-generated from PCI address)
   */
  name?: string;
}

/**
 * Type of PCI device
 */
export type DeviceType = 'gpu' | 'pci';

export type DeviceListResponse = Array<Device>;

export type DeviceListAvailableResponse = Array<AvailableDevice>;

export interface DeviceCreateParams {
  /**
   * PCI address of the device (required, e.g., "0000:a2:00.0")
   */
  pci_address: string;

  /**
   * Optional globally unique device name. If not provided, a name is auto-generated
   * from the PCI address (e.g., "pci-0000-a2-00-0")
   */
  name?: string;
}

export declare namespace Devices {
  export {
    type AvailableDevice as AvailableDevice,
    type Device as Device,
    type DeviceType as DeviceType,
    type DeviceListResponse as DeviceListResponse,
    type DeviceListAvailableResponse as DeviceListAvailableResponse,
    type DeviceCreateParams as DeviceCreateParams,
  };
}
