package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/ingress"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/middleware"
	"github.com/onkernel/hypeman/lib/volumes"
)

// InstanceResolver adapts instances.Manager to middleware.ResourceResolver.
type InstanceResolver struct {
	Manager instances.Manager
}

func (r InstanceResolver) Resolve(ctx context.Context, idOrName string) (string, any, error) {
	inst, err := r.Manager.GetInstance(ctx, idOrName)
	if err != nil {
		return "", nil, err
	}
	return inst.Id, inst, nil
}

// VolumeResolver adapts volumes.Manager to middleware.ResourceResolver.
type VolumeResolver struct {
	Manager volumes.Manager
}

func (r VolumeResolver) Resolve(ctx context.Context, idOrName string) (string, any, error) {
	// Try by ID first, then by name
	vol, err := r.Manager.GetVolume(ctx, idOrName)
	if errors.Is(err, volumes.ErrNotFound) {
		vol, err = r.Manager.GetVolumeByName(ctx, idOrName)
	}
	if err != nil {
		return "", nil, err
	}
	return vol.Id, vol, nil
}

// IngressResolver adapts ingress.Manager to middleware.ResourceResolver.
type IngressResolver struct {
	Manager ingress.Manager
}

func (r IngressResolver) Resolve(ctx context.Context, idOrName string) (string, any, error) {
	ing, err := r.Manager.Get(ctx, idOrName)
	if err != nil {
		return "", nil, err
	}
	return ing.ID, ing, nil
}

// ImageResolver adapts images.Manager to middleware.ResourceResolver.
// Note: Images are looked up by name (OCI reference), not ID.
type ImageResolver struct {
	Manager images.Manager
}

func (r ImageResolver) Resolve(ctx context.Context, name string) (string, any, error) {
	img, err := r.Manager.GetImage(ctx, name)
	if err != nil {
		return "", nil, err
	}
	return img.Name, img, nil
}

// NewResolvers creates Resolvers from the ApiService managers.
func (s *ApiService) NewResolvers() middleware.Resolvers {
	return middleware.Resolvers{
		Instance: InstanceResolver{Manager: s.InstanceManager},
		Volume:   VolumeResolver{Manager: s.VolumeManager},
		Ingress:  IngressResolver{Manager: s.IngressManager},
		Image:    ImageResolver{Manager: s.ImageManager},
	}
}

// ResolverErrorResponder handles resolver errors by writing appropriate HTTP responses.
func ResolverErrorResponder(w http.ResponseWriter, err error, lookup string) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case errors.Is(err, instances.ErrNotFound),
		errors.Is(err, volumes.ErrNotFound),
		errors.Is(err, ingress.ErrNotFound),
		errors.Is(err, images.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found","message":"resource not found"}`))

	case errors.Is(err, instances.ErrAmbiguousName),
		errors.Is(err, volumes.ErrAmbiguousName),
		errors.Is(err, ingress.ErrAmbiguousName):
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"ambiguous","message":"multiple resources match, use full ID"}`))

	case errors.Is(err, images.ErrInvalidName):
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":"invalid_name","message":"invalid image reference"}`))

	default:
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":"internal_error","message":"failed to resolve resource"}`))
	}
}
