package api

import (
	"context"
	"errors"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/kernel/hypeman/lib/imagepush"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
)

const pushRetryAfterSeconds = 2

func (s *ApiService) CreatePush(ctx context.Context, request oapi.CreatePushRequestObject) (oapi.CreatePushResponseObject, error) {
	if request.Body == nil {
		return oapi.CreatePush400JSONResponse{
			Code:    "invalid_request",
			Message: "request body is required",
		}, nil
	}

	log := logger.FromContext(ctx)

	domainReq := imagepush.PushRequest{
		Image:       request.Body.Image,
		Target:      request.Body.Target,
		Credentials: pushCredentialsToAuthn(request.Body.Credentials),
	}
	if request.Body.Insecure != nil {
		domainReq.Insecure = *request.Body.Insecure
	}

	push, err := s.PushManager.CreatePush(ctx, domainReq)
	if err != nil {
		switch {
		case errors.Is(err, images.ErrInvalidName):
			return oapi.CreatePush400JSONResponse{
				Code:    "invalid_name",
				Message: err.Error(),
			}, nil
		case errors.Is(err, imagepush.ErrInvalidTarget):
			return oapi.CreatePush400JSONResponse{
				Code:    "invalid_target",
				Message: err.Error(),
			}, nil
		case errors.Is(err, images.ErrNotFound):
			return oapi.CreatePush404JSONResponse{
				Code:    "not_found",
				Message: "image not found",
			}, nil
		case errors.Is(err, imagepush.ErrNotFound):
			return oapi.CreatePush409JSONResponse{
				Code:    "conflict",
				Message: err.Error(),
			}, nil
		case errors.Is(err, imagepush.ErrCredentialConflict):
			return oapi.CreatePush409JSONResponse{
				Code:    "credential_conflict",
				Message: err.Error(),
			}, nil
		case errors.Is(err, imagepush.ErrImageNotReady):
			return oapi.CreatePush409JSONResponse{
				Code:    "image_not_ready",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to create push", "error", err)
			return oapi.CreatePush500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create push",
			}, nil
		}
	}

	return oapi.CreatePush202JSONResponse{
		Body: pushToOAPI(*push),
		Headers: oapi.CreatePush202ResponseHeaders{
			Location:   "/pushes/" + push.ID,
			RetryAfter: pushRetryAfterSeconds,
		},
	}, nil
}

func (s *ApiService) GetPush(ctx context.Context, request oapi.GetPushRequestObject) (oapi.GetPushResponseObject, error) {
	log := logger.FromContext(ctx)

	push, err := s.PushManager.GetPush(ctx, request.Id)
	if err != nil {
		if errors.Is(err, imagepush.ErrNotFound) {
			return oapi.GetPush404JSONResponse{
				Code:    "not_found",
				Message: "push not found",
			}, nil
		}
		log.ErrorContext(ctx, "failed to get push", "error", err)
		return oapi.GetPush500JSONResponse{
			Code:    "internal_error",
			Message: "failed to get push",
		}, nil
	}

	return oapi.GetPush200JSONResponse(pushToOAPI(*push)), nil
}

func (s *ApiService) ListPushes(ctx context.Context, request oapi.ListPushesRequestObject) (oapi.ListPushesResponseObject, error) {
	log := logger.FromContext(ctx)

	pushes, err := s.PushManager.ListPushes(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list pushes", "error", err)
		return oapi.ListPushes500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list pushes",
		}, nil
	}

	out := make([]oapi.Push, 0, len(pushes))
	for _, push := range pushes {
		out = append(out, pushToOAPI(push))
	}
	return oapi.ListPushes200JSONResponse(out), nil
}

// pushCredentialsToAuthn maps API credentials to the go-containerregistry
// auth config. Returns nil when absent or empty so the push falls back to
// the server's default credential resolution — an empty credentials object
// must not mask the keychain.
func pushCredentialsToAuthn(creds *oapi.PushCredentials) *authn.AuthConfig {
	if creds == nil {
		return nil
	}
	cfg := &authn.AuthConfig{}
	if creds.Username != nil {
		cfg.Username = *creds.Username
	}
	if creds.Password != nil {
		cfg.Password = *creds.Password
	}
	if creds.RegistryToken != nil {
		cfg.RegistryToken = *creds.RegistryToken
	}
	if cfg.Username == "" && cfg.Password == "" && cfg.RegistryToken == "" {
		return nil
	}
	return cfg
}

func pushToOAPI(push imagepush.Push) oapi.Push {
	out := oapi.Push{
		Id:            push.ID,
		Image:         push.Image,
		Digest:        push.Digest,
		Target:        push.Target,
		Status:        oapi.PushStatus(push.Status),
		QueuePosition: push.QueuePosition,
		Error:         push.Error,
		CreatedAt:     push.CreatedAt,
		CompletedAt:   push.CompletedAt,
	}
	if push.Status == imagepush.StatusPushed {
		layers := push.Layers
		out.Layers = &layers
		bytes := push.Bytes
		out.Bytes = &bytes
	}
	return out
}
