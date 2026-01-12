package api

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
)

// ListIngresses lists all ingress resources
func (s *ApiService) ListIngresses(ctx context.Context, request oapi.ListIngressesRequestObject) (oapi.ListIngressesResponseObject, error) {
	log := logger.FromContext(ctx)

	ingresses, err := s.IngressManager.List(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list ingresses", "error", err)
		return oapi.ListIngresses500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list ingresses",
		}, nil
	}

	oapiIngresses := make([]oapi.Ingress, len(ingresses))
	for i, ing := range ingresses {
		oapiIngresses[i] = ingressToOAPI(ing)
	}

	return oapi.ListIngresses200JSONResponse(oapiIngresses), nil
}

// CreateIngress creates a new ingress resource
func (s *ApiService) CreateIngress(ctx context.Context, request oapi.CreateIngressRequestObject) (oapi.CreateIngressResponseObject, error) {
	log := logger.FromContext(ctx)

	// Convert OAPI request to domain request
	domainReq := ingress.CreateIngressRequest{
		Name:  request.Body.Name,
		Rules: make([]ingress.IngressRule, len(request.Body.Rules)),
	}

	for i, rule := range request.Body.Rules {
		matchPort := 80
		if rule.Match.Port != nil {
			matchPort = *rule.Match.Port
		}
		tlsEnabled := false
		if rule.Tls != nil {
			tlsEnabled = *rule.Tls
		}
		redirectHTTP := false
		if rule.RedirectHttp != nil {
			redirectHTTP = *rule.RedirectHttp
		}
		domainReq.Rules[i] = ingress.IngressRule{
			Match: ingress.IngressMatch{
				Hostname: rule.Match.Hostname,
				Port:     matchPort,
			},
			Target: ingress.IngressTarget{
				Instance: rule.Target.Instance,
				Port:     rule.Target.Port,
			},
			TLS:          tlsEnabled,
			RedirectHTTP: redirectHTTP,
		}
	}

	ing, err := s.IngressManager.Create(ctx, domainReq)
	if err != nil {
		switch {
		case errors.Is(err, ingress.ErrInvalidRequest):
			return oapi.CreateIngress400JSONResponse{
				Code:    "bad_request",
				Message: err.Error(),
			}, nil
		case errors.Is(err, ingress.ErrAlreadyExists):
			return oapi.CreateIngress409JSONResponse{
				Code:    "already_exists",
				Message: err.Error(),
			}, nil
		case errors.Is(err, ingress.ErrHostnameInUse):
			return oapi.CreateIngress409JSONResponse{
				Code:    "hostname_in_use",
				Message: err.Error(),
			}, nil
		case errors.Is(err, ingress.ErrPortInUse):
			return oapi.CreateIngress409JSONResponse{
				Code:    "port_in_use",
				Message: err.Error(),
			}, nil
		case errors.Is(err, ingress.ErrInstanceNotFound):
			return oapi.CreateIngress400JSONResponse{
				Code:    "instance_not_found",
				Message: err.Error(),
			}, nil
		case errors.Is(err, ingress.ErrDomainNotAllowed):
			return oapi.CreateIngress400JSONResponse{
				Code:    "domain_not_allowed",
				Message: err.Error(),
			}, nil
		case errors.Is(err, ingress.ErrConfigValidationFailed):
			log.ErrorContext(ctx, "failed to create ingress", "error", err, "name", request.Body.Name)
			return oapi.CreateIngress400JSONResponse{
				Code:    "config_validation_failed",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to create ingress", "error", err, "name", request.Body.Name)
			return oapi.CreateIngress500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create ingress",
			}, nil
		}
	}

	return oapi.CreateIngress201JSONResponse(ingressToOAPI(*ing)), nil
}

// GetIngress gets ingress details by ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) GetIngress(ctx context.Context, request oapi.GetIngressRequestObject) (oapi.GetIngressResponseObject, error) {
	ing := mw.GetResolvedIngress[ingress.Ingress](ctx)
	if ing == nil {
		return oapi.GetIngress500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	return oapi.GetIngress200JSONResponse(ingressToOAPI(*ing)), nil
}

// DeleteIngress deletes an ingress by ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) DeleteIngress(ctx context.Context, request oapi.DeleteIngressRequestObject) (oapi.DeleteIngressResponseObject, error) {
	ing := mw.GetResolvedIngress[ingress.Ingress](ctx)
	if ing == nil {
		return oapi.DeleteIngress500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	err := s.IngressManager.Delete(ctx, ing.ID)
	if err != nil {
		log.ErrorContext(ctx, "failed to delete ingress", "error", err)
		return oapi.DeleteIngress500JSONResponse{
			Code:    "internal_error",
			Message: "failed to delete ingress",
		}, nil
	}

	return oapi.DeleteIngress204Response{}, nil
}

// ingressToOAPI converts a domain Ingress to the OAPI type
func ingressToOAPI(ing ingress.Ingress) oapi.Ingress {
	rules := make([]oapi.IngressRule, len(ing.Rules))
	for i, rule := range ing.Rules {
		port := rule.Match.GetPort()
		tls := rule.TLS
		redirectHTTP := rule.RedirectHTTP
		rules[i] = oapi.IngressRule{
			Match: oapi.IngressMatch{
				Hostname: rule.Match.Hostname,
				Port:     &port,
			},
			Target: oapi.IngressTarget{
				Instance: rule.Target.Instance,
				Port:     rule.Target.Port,
			},
			Tls:          &tls,
			RedirectHttp: &redirectHTTP,
		}
	}

	return oapi.Ingress{
		Id:        ing.ID,
		Name:      ing.Name,
		Rules:     rules,
		CreatedAt: ing.CreatedAt,
	}
}
