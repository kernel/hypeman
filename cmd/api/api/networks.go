package api

import (
	"context"
	"errors"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/oapi"
)

// ListNetworks lists all networks
func (s *ApiService) ListNetworks(ctx context.Context, request oapi.ListNetworksRequestObject) (oapi.ListNetworksResponseObject, error) {
	log := logger.FromContext(ctx)

	domainNetworks, err := s.NetworkManager.ListNetworks(ctx)
	if err != nil {
		log.Error("failed to list networks", "error", err)
		return oapi.ListNetworks500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list networks",
		}, nil
	}

	oapiNetworks := make([]oapi.Network, len(domainNetworks))
	for i, net := range domainNetworks {
		oapiNetworks[i] = networkToOAPI(net)
	}

	return oapi.ListNetworks200JSONResponse(oapiNetworks), nil
}

// CreateNetwork creates a new network
func (s *ApiService) CreateNetwork(ctx context.Context, request oapi.CreateNetworkRequestObject) (oapi.CreateNetworkResponseObject, error) {
	log := logger.FromContext(ctx)

	// Default isolated to true if not specified
	isolated := true
	if request.Body.Isolated != nil {
		isolated = *request.Body.Isolated
	}

	domainReq := network.CreateNetworkRequest{
		Name:     request.Body.Name,
		Subnet:   request.Body.Subnet,
		Isolated: isolated,
	}

	net, err := s.NetworkManager.CreateNetwork(ctx, domainReq)
	if err != nil {
		switch {
		case errors.Is(err, network.ErrAlreadyExists):
			return oapi.CreateNetwork400JSONResponse{
				Code:    "already_exists",
				Message: err.Error(),
			}, nil
		case errors.Is(err, network.ErrInvalidName):
			return oapi.CreateNetwork400JSONResponse{
				Code:    "invalid_name",
				Message: err.Error(),
			}, nil
		case errors.Is(err, network.ErrInvalidSubnet):
			return oapi.CreateNetwork400JSONResponse{
				Code:    "invalid_subnet",
				Message: err.Error(),
			}, nil
		case errors.Is(err, network.ErrSubnetOverlap):
			return oapi.CreateNetwork400JSONResponse{
				Code:    "subnet_overlap",
				Message: err.Error(),
			}, nil
		default:
			log.Error("failed to create network", "error", err, "name", request.Body.Name)
			return oapi.CreateNetwork500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create network",
			}, nil
		}
	}

	return oapi.CreateNetwork201JSONResponse(networkToOAPI(*net)), nil
}

// GetNetwork gets network details
func (s *ApiService) GetNetwork(ctx context.Context, request oapi.GetNetworkRequestObject) (oapi.GetNetworkResponseObject, error) {
	log := logger.FromContext(ctx)

	net, err := s.NetworkManager.GetNetwork(ctx, request.Name)
	if err != nil {
		switch {
		case errors.Is(err, network.ErrNotFound):
			return oapi.GetNetwork404JSONResponse{
				Code:    "not_found",
				Message: "network not found",
			}, nil
		default:
			log.Error("failed to get network", "error", err, "name", request.Name)
			return oapi.GetNetwork500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get network",
			}, nil
		}
	}

	return oapi.GetNetwork200JSONResponse(networkToOAPI(*net)), nil
}

// DeleteNetwork deletes a network
func (s *ApiService) DeleteNetwork(ctx context.Context, request oapi.DeleteNetworkRequestObject) (oapi.DeleteNetworkResponseObject, error) {
	log := logger.FromContext(ctx)

	err := s.NetworkManager.DeleteNetwork(ctx, request.Name)
	if err != nil {
		switch {
		case errors.Is(err, network.ErrNotFound):
			return oapi.DeleteNetwork404JSONResponse{
				Code:    "not_found",
				Message: "network not found",
			}, nil
		case errors.Is(err, network.ErrDefaultNetwork):
			return oapi.DeleteNetwork400JSONResponse{
				Code:    "cannot_delete_default",
				Message: "cannot delete default network",
			}, nil
		case errors.Is(err, network.ErrNetworkInUse):
			return oapi.DeleteNetwork409JSONResponse{
				Code:    "network_in_use",
				Message: err.Error(),
			}, nil
		default:
			log.Error("failed to delete network", "error", err, "name", request.Name)
			return oapi.DeleteNetwork500JSONResponse{
				Code:    "internal_error",
				Message: "failed to delete network",
			}, nil
		}
	}

	return oapi.DeleteNetwork204Response{}, nil
}

// networkToOAPI converts domain Network to OAPI Network
func networkToOAPI(net network.Network) oapi.Network {
	createdAt := net.CreatedAt
	oapiNet := oapi.Network{
		Name:      net.Name,
		Subnet:    net.Subnet,
		Gateway:   net.Gateway,
		Bridge:    net.Bridge,
		Isolated:  net.Isolated,
		DnsDomain: &net.DNSDomain,
		Default:   net.Default,
		CreatedAt: &createdAt,
	}

	return oapiNet
}

