package api

import (
	"context"

	"github.com/kernel/hypeman/lib/oapi"
)

// GetHealth implements health check endpoint
func (s *ApiService) GetHealth(ctx context.Context, request oapi.GetHealthRequestObject) (oapi.GetHealthResponseObject, error) {
	return oapi.GetHealth200JSONResponse{
		Status: oapi.Ok,
	}, nil
}

