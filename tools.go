//go:build tools

package tools

// This file declares tool dependencies so they are tracked in go.mod.
// It uses a build constraint to prevent it from being compiled.
//
// To regenerate proto files, use: make generate-grpc
// The generators will use the versions pinned in go.mod.

import (
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
