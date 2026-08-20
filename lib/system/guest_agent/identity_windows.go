//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"fmt"

	pb "github.com/kernel/hypeman/lib/guest"
	"golang.org/x/sys/windows/registry"
)

func (s *guestServer) RebindIdentity(_ context.Context, req *pb.RebindIdentityRequest) (*pb.RebindIdentityResponse, error) {
	if req.InstanceId == "" {
		return nil, fmt.Errorf("instance id is required")
	}

	machineID, err := newWindowsMachineID()
	if err != nil {
		return nil, fmt.Errorf("generate Windows machine id: %w", err)
	}
	cryptography, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("open Windows machine identity: %w", err)
	}
	defer cryptography.Close()
	if err := cryptography.SetStringValue("MachineGuid", machineID); err != nil {
		return nil, fmt.Errorf("set Windows machine identity: %w", err)
	}

	marker, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Kernel\Hypeman`, registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("open Hypeman identity marker: %w", err)
	}
	defer marker.Close()
	if err := marker.SetStringValue("InstanceID", req.InstanceId); err != nil {
		return nil, fmt.Errorf("set Hypeman instance identity: %w", err)
	}
	return &pb.RebindIdentityResponse{MachineId: machineID}, nil
}

func newWindowsMachineID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
