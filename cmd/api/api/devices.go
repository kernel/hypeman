package api

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/oapi"
)

// ListDevices returns all registered devices
func (s *ApiService) ListDevices(ctx context.Context, request oapi.ListDevicesRequestObject) (oapi.ListDevicesResponseObject, error) {
	deviceList, err := s.DeviceManager.ListDevices(ctx)
	if err != nil {
		return oapi.ListDevices500JSONResponse{
			Code:    "internal_error",
			Message: err.Error(),
		}, nil
	}

	result := make([]oapi.Device, len(deviceList))
	for i, d := range deviceList {
		result[i] = deviceToOAPI(d)
	}

	return oapi.ListDevices200JSONResponse(result), nil
}

// ListAvailableDevices discovers passthrough-capable devices on the host
func (s *ApiService) ListAvailableDevices(ctx context.Context, request oapi.ListAvailableDevicesRequestObject) (oapi.ListAvailableDevicesResponseObject, error) {
	available, err := s.DeviceManager.ListAvailableDevices(ctx)
	if err != nil {
		return oapi.ListAvailableDevices500JSONResponse{
			Code:    "internal_error",
			Message: err.Error(),
		}, nil
	}

	result := make([]oapi.AvailableDevice, len(available))
	for i, d := range available {
		result[i] = availableDeviceToOAPI(d)
	}

	return oapi.ListAvailableDevices200JSONResponse(result), nil
}

// CreateDevice registers a new device for passthrough
func (s *ApiService) CreateDevice(ctx context.Context, request oapi.CreateDeviceRequestObject) (oapi.CreateDeviceResponseObject, error) {
	var name string
	if request.Body.Name != nil {
		name = *request.Body.Name
	}
	req := devices.CreateDeviceRequest{
		Name:       name,
		PCIAddress: request.Body.PciAddress,
	}

	device, err := s.DeviceManager.CreateDevice(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, devices.ErrInvalidName):
			return oapi.CreateDevice400JSONResponse{
				Code:    "invalid_name",
				Message: err.Error(),
			}, nil
		case errors.Is(err, devices.ErrInvalidPCIAddress):
			return oapi.CreateDevice400JSONResponse{
				Code:    "invalid_pci_address",
				Message: err.Error(),
			}, nil
		case errors.Is(err, devices.ErrDeviceNotFound):
			return oapi.CreateDevice404JSONResponse{
				Code:    "device_not_found",
				Message: err.Error(),
			}, nil
		case errors.Is(err, devices.ErrAlreadyExists), errors.Is(err, devices.ErrNameExists):
			return oapi.CreateDevice409JSONResponse{
				Code:    "conflict",
				Message: err.Error(),
			}, nil
		default:
			return oapi.CreateDevice500JSONResponse{
				Code:    "internal_error",
				Message: err.Error(),
			}, nil
		}
	}

	return oapi.CreateDevice201JSONResponse(deviceToOAPI(*device)), nil
}

// GetDevice returns a device by ID or name
func (s *ApiService) GetDevice(ctx context.Context, request oapi.GetDeviceRequestObject) (oapi.GetDeviceResponseObject, error) {
	device, err := s.DeviceManager.GetDevice(ctx, request.Id)
	if err != nil {
		if errors.Is(err, devices.ErrNotFound) {
			return oapi.GetDevice404JSONResponse{
				Code:    "not_found",
				Message: "device not found",
			}, nil
		}
		return oapi.GetDevice500JSONResponse{
			Code:    "internal_error",
			Message: err.Error(),
		}, nil
	}

	return oapi.GetDevice200JSONResponse(deviceToOAPI(*device)), nil
}

// DeleteDevice unregisters a device
func (s *ApiService) DeleteDevice(ctx context.Context, request oapi.DeleteDeviceRequestObject) (oapi.DeleteDeviceResponseObject, error) {
	err := s.DeviceManager.DeleteDevice(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, devices.ErrNotFound):
			return oapi.DeleteDevice404JSONResponse{
				Code:    "not_found",
				Message: "device not found",
			}, nil
		case errors.Is(err, devices.ErrInUse):
			return oapi.DeleteDevice409JSONResponse{
				Code:    "in_use",
				Message: "device is attached to an instance",
			}, nil
		default:
			return oapi.DeleteDevice500JSONResponse{
				Code:    "internal_error",
				Message: err.Error(),
			}, nil
		}
	}

	return oapi.DeleteDevice204Response{}, nil
}

// Helper functions

func deviceToOAPI(d devices.Device) oapi.Device {
	deviceType := oapi.DeviceType(d.Type)
	return oapi.Device{
		Id:          d.Id,
		Name:        &d.Name,
		Type:        deviceType,
		PciAddress:  d.PCIAddress,
		VendorId:    d.VendorID,
		DeviceId:    d.DeviceID,
		IommuGroup:  d.IOMMUGroup,
		BoundToVfio: d.BoundToVFIO,
		AttachedTo:  d.AttachedTo,
		CreatedAt:   d.CreatedAt,
	}
}

func availableDeviceToOAPI(d devices.AvailableDevice) oapi.AvailableDevice {
	return oapi.AvailableDevice{
		PciAddress:    d.PCIAddress,
		VendorId:      d.VendorID,
		DeviceId:      d.DeviceID,
		VendorName:    &d.VendorName,
		DeviceName:    &d.DeviceName,
		IommuGroup:    d.IOMMUGroup,
		CurrentDriver: d.CurrentDriver,
	}
}


