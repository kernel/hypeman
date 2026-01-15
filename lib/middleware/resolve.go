// Package middleware provides HTTP middleware for the hypeman API.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// HypervisorTyper is implemented by resources that have a hypervisor type.
// This allows the middleware to enrich logs/traces without importing the instances package.
type HypervisorTyper interface {
	GetHypervisorType() string
}

// ResourceResolver is implemented by managers that support lookup by ID, name, or prefix.
type ResourceResolver interface {
	// Resolve looks up a resource by ID, name, or ID prefix.
	// Returns the resolved ID, the resource, and any error.
	// Should return ErrNotFound if not found, ErrAmbiguousName if prefix matches multiple.
	Resolve(ctx context.Context, idOrName string) (id string, resource any, err error)
}

// resolvedResourceKey is the context key for storing the resolved resource.
type resolvedResourceKey struct{ resourceType string }

// ResolvedResource holds the resolved resource ID and value.
type ResolvedResource struct {
	ID       string
	Resource any
}

// Resolvers holds resolvers for different resource types.
type Resolvers struct {
	Instance ResourceResolver
	Volume   ResourceResolver
	Ingress  ResourceResolver
	Image    ResourceResolver
}

// ErrorResponder handles resolver errors by writing HTTP responses.
type ErrorResponder func(w http.ResponseWriter, err error, lookup string)

// ResolveResource creates middleware that resolves resource IDs before handlers run.
// It detects the resource type from the URL path and uses the appropriate resolver.
// The resolved resource is stored in context and the logger is enriched with the ID.
//
// Supported paths:
//   - /instances/{id}/* -> uses Instance resolver
//   - /volumes/{id}/* -> uses Volume resolver
//   - /ingresses/{id}/* -> uses Ingress resolver
//   - /images/{name}/* -> uses Image resolver (by name, not ID)
func ResolveResource(resolvers Resolvers, errResponder ErrorResponder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			path := r.URL.Path

			// Determine resource type and resolver based on path
			var resolver ResourceResolver
			var resourceType string
			var paramName string

			switch {
			case strings.HasPrefix(path, "/instances/"):
				resolver = resolvers.Instance
				resourceType = "instance"
				paramName = "id"
			case strings.HasPrefix(path, "/volumes/"):
				resolver = resolvers.Volume
				resourceType = "volume"
				paramName = "id"
			case strings.HasPrefix(path, "/ingresses/"):
				resolver = resolvers.Ingress
				resourceType = "ingress"
				paramName = "id"
			case strings.HasPrefix(path, "/images/"):
				resolver = resolvers.Image
				resourceType = "image"
				paramName = "name"
			default:
				// No resource to resolve (e.g., list endpoints, health)
				next.ServeHTTP(w, r)
				return
			}

			// Skip if no resolver configured for this resource type
			if resolver == nil {
				next.ServeHTTP(w, r)
				return
			}

			// Get the ID parameter from the URL
			idOrName := chi.URLParam(r, paramName)
			if idOrName == "" {
				// No ID in path (e.g., list or create endpoint)
				next.ServeHTTP(w, r)
				return
			}

			// Resolve the resource
			resolvedID, resource, err := resolver.Resolve(ctx, idOrName)
			if err != nil {
				errResponder(w, err, idOrName)
				return
			}

			// Store resolved resource in context
			ctx = context.WithValue(ctx, resolvedResourceKey{resourceType}, ResolvedResource{
				ID:       resolvedID,
				Resource: resource,
			})

			// Enrich logger with resource-specific key
			// Use "image_name" for images (keyed by OCI reference), "<type>_id" for others
			logKey := resourceType + "_id"
			if resourceType == "image" {
				logKey = "image_name"
			}
			log := logger.FromContext(ctx).With(logKey, resolvedID)

			// For instances, also add hypervisor type to logs and traces
			if resourceType == "instance" {
				if hvTyper, ok := resource.(HypervisorTyper); ok {
					hvType := hvTyper.GetHypervisorType()
					if hvType != "" {
						log = log.With("hypervisor", hvType)

						// Add to trace span if one exists
						span := trace.SpanFromContext(ctx)
						if span.IsRecording() {
							span.SetAttributes(attribute.String("hypervisor", hvType))
						}
					}
				}
			}

			ctx = logger.AddToContext(ctx, log)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetResolvedInstance retrieves the resolved instance from context.
// Returns nil if not found or wrong type.
func GetResolvedInstance[T any](ctx context.Context) *T {
	return getResolved[T](ctx, "instance")
}

// GetResolvedVolume retrieves the resolved volume from context.
// Returns nil if not found or wrong type.
func GetResolvedVolume[T any](ctx context.Context) *T {
	return getResolved[T](ctx, "volume")
}

// GetResolvedIngress retrieves the resolved ingress from context.
// Returns nil if not found or wrong type.
func GetResolvedIngress[T any](ctx context.Context) *T {
	return getResolved[T](ctx, "ingress")
}

// GetResolvedImage retrieves the resolved image from context.
// Returns nil if not found or wrong type.
func GetResolvedImage[T any](ctx context.Context) *T {
	return getResolved[T](ctx, "image")
}

// GetResolvedID retrieves just the resolved ID for a resource type.
func GetResolvedID(ctx context.Context, resourceType string) string {
	if resolved, ok := ctx.Value(resolvedResourceKey{resourceType}).(ResolvedResource); ok {
		return resolved.ID
	}
	return ""
}

// getResolved is a generic helper to extract typed resources from context.
func getResolved[T any](ctx context.Context, resourceType string) *T {
	resolved, ok := ctx.Value(resolvedResourceKey{resourceType}).(ResolvedResource)
	if !ok {
		return nil
	}

	// Handle pointer types
	if typed, ok := resolved.Resource.(*T); ok {
		return typed
	}

	// Handle value types
	if typed, ok := resolved.Resource.(T); ok {
		return &typed
	}

	return nil
}

// Test helpers for setting resolved resources in context (used by tests)

// WithResolvedInstance returns a context with the given instance set as resolved.
func WithResolvedInstance(ctx context.Context, id string, inst any) context.Context {
	return context.WithValue(ctx, resolvedResourceKey{"instance"}, ResolvedResource{ID: id, Resource: inst})
}

// WithResolvedVolume returns a context with the given volume set as resolved.
func WithResolvedVolume(ctx context.Context, id string, vol any) context.Context {
	return context.WithValue(ctx, resolvedResourceKey{"volume"}, ResolvedResource{ID: id, Resource: vol})
}

// WithResolvedIngress returns a context with the given ingress set as resolved.
func WithResolvedIngress(ctx context.Context, id string, ing any) context.Context {
	return context.WithValue(ctx, resolvedResourceKey{"ingress"}, ResolvedResource{ID: id, Resource: ing})
}

// WithResolvedImage returns a context with the given image set as resolved.
func WithResolvedImage(ctx context.Context, id string, img any) context.Context {
	return context.WithValue(ctx, resolvedResourceKey{"image"}, ResolvedResource{ID: id, Resource: img})
}
