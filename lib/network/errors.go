package network

import "errors"

var (
	// ErrNotFound is returned when a network is not found
	ErrNotFound = errors.New("network not found")

	// ErrAlreadyExists is returned when a network already exists
	ErrAlreadyExists = errors.New("network already exists")

	// ErrDefaultNetwork is returned when attempting to delete/modify default network
	ErrDefaultNetwork = errors.New("cannot delete or modify default network")

	// ErrNameExists is returned when an instance name already exists in a network
	ErrNameExists = errors.New("instance name already exists in network")

	// ErrInvalidSubnet is returned when subnet is invalid
	ErrInvalidSubnet = errors.New("invalid subnet")

	// ErrSubnetOverlap is returned when subnets overlap
	ErrSubnetOverlap = errors.New("subnet overlaps with existing network")

	// ErrNetworkInUse is returned when trying to delete a network with active instances
	ErrNetworkInUse = errors.New("network has active instances")

	// ErrInvalidName is returned when network name is invalid
	ErrInvalidName = errors.New("invalid network name")
)

