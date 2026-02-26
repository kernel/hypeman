// Provide HTTP middleware functionality to validate that incoming requests conform to a given OpenAPI 3.x specification.
//
// This provides middleware for any `net/http` conforming HTTP Server.
//
// This package is a lightweight wrapper over https://pkg.go.dev/github.com/getkin/kin-openapi/openapi3filter from https://pkg.go.dev/github.com/getkin/kin-openapi.
//
// This is _intended_ to be used with code that's generated through https://pkg.go.dev/github.com/oapi-codegen/oapi-codegen, but should work otherwise.
package nethttpmiddleware

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// ErrorHandler is called when there is an error in validation
//
// If both an `ErrorHandlerWithOpts` and `ErrorHandler` are set, the `ErrorHandlerWithOpts` takes precedence.
//
// Deprecated: it's recommended you migrate to the ErrorHandlerWithOpts, as it provides more control over how to handle an error that occurs, including giving direct access to the `error` itself. There are no plans to remove this method.
type ErrorHandler func(w http.ResponseWriter, message string, statusCode int)

// ErrorHandlerWithOpts is called when there is an error in validation, with more information about the `error` that occurred and which request is currently being processed.
//
// There are a number of known types that the `error` can be:
//
// - `*openapi3filter.SecurityRequirementsError` - if the `AuthenticationFunc` has failed to authenticate the request
// - `*openapi3filter.RequestError` - if a bad request has been made
//
// Additionally, if you have set `openapi3filter.Options#MultiError`:
//
// - `openapi3.MultiError` (https://pkg.go.dev/github.com/getkin/kin-openapi/openapi3#MultiError)
//
// If both an `ErrorHandlerWithOpts` and `ErrorHandler` are set, the `ErrorHandlerWithOpts` takes precedence.
//
// NOTE that this should ideally be used instead of ErrorHandler
type ErrorHandlerWithOpts func(ctx context.Context, err error, w http.ResponseWriter, r *http.Request, opts ErrorHandlerOpts)

// ErrorHandlerOpts contains additional options that are passed to the `ErrorHandlerWithOpts` function in the case of an error being returned by the middleware
type ErrorHandlerOpts struct {
	// StatusCode indicates the HTTP Status Code that the OpenAPI validation middleware _suggests_ is returned to the user.
	//
	// NOTE that this is very much a suggestion, and can be overridden if you believe you have a better approach.
	StatusCode int

	// MatchedRoute is the underlying path that this request is being matched against.
	//
	// This is the route according to the OpenAPI validation middleware, and can be used in addition to/instead of the `http.Request`
	//
	// NOTE that this will be nil if there is no matched route (i.e. a request has been sent to an endpoint not in the OpenAPI spec)
	MatchedRoute *ErrorHandlerOptsMatchedRoute
}

type ErrorHandlerOptsMatchedRoute struct {
	// Route indicates the Route that this error is received by.
	//
	// This can be used in addition to/instead of the `http.Request`.
	Route *routers.Route

	// PathParams are any path parameters that are determined from the request.
	//
	// This can be used in addition to/instead of the `http.Request`.
	PathParams map[string]string
}

// MultiErrorHandler is called when the OpenAPI filter returns an openapi3.MultiError (https://pkg.go.dev/github.com/getkin/kin-openapi/openapi3#MultiError)
type MultiErrorHandler func(openapi3.MultiError) (int, error)

// Options allows configuring the OapiRequestValidator.
type Options struct {
	// Options contains any configuration for the underlying `openapi3filter`
	Options openapi3filter.Options
	// ErrorHandler is called when a validation error occurs.
	//
	// If both an `ErrorHandlerWithOpts` and `ErrorHandler` are set, the `ErrorHandlerWithOpts` takes precedence.
	//
	// If not provided, `http.Error` will be called
	ErrorHandler ErrorHandler

	// ErrorHandlerWithOpts is called when there is an error in validation.
	//
	// If both an `ErrorHandlerWithOpts` and `ErrorHandler` are set, the `ErrorHandlerWithOpts` takes precedence.
	ErrorHandlerWithOpts ErrorHandlerWithOpts

	// MultiErrorHandler is called when there is an openapi3.MultiError (https://pkg.go.dev/github.com/getkin/kin-openapi/openapi3#MultiError) returned by the `openapi3filter`.
	//
	// If not provided `defaultMultiErrorHandler` will be used.
	//
	// Does not get called when using `ErrorHandlerWithOpts`
	MultiErrorHandler MultiErrorHandler
	// SilenceServersWarning allows silencing a warning for https://github.com/deepmap/oapi-codegen/issues/882 that reports when an OpenAPI spec has `spec.Servers != nil`
	SilenceServersWarning bool
	// DoNotValidateServers ensures that there is no Host validation performed (see `SilenceServersWarning` and https://github.com/deepmap/oapi-codegen/issues/882 for more details)
	DoNotValidateServers bool
}

// OapiRequestValidator Creates the middleware to validate that incoming requests match the given OpenAPI 3.x spec, with a default set of configuration.
func OapiRequestValidator(spec *openapi3.T) func(next http.Handler) http.Handler {
	return OapiRequestValidatorWithOptions(spec, nil)
}

// OapiRequestValidatorWithOptions Creates the middleware to validate that incoming requests match the given OpenAPI 3.x spec, allowing explicit configuration.
//
// NOTE that this may panic if the OpenAPI spec isn't valid, or if it cannot be used to create the middleware
func OapiRequestValidatorWithOptions(spec *openapi3.T, options *Options) func(next http.Handler) http.Handler {
	if options != nil && options.DoNotValidateServers {
		spec.Servers = nil
	}

	if spec.Servers != nil && (options == nil || !options.SilenceServersWarning) {
		log.Println("WARN: OapiRequestValidatorWithOptions called with an OpenAPI spec that has `Servers` set. This may lead to an HTTP 400 with `no matching operation was found` when sending a valid request, as the validator performs `Host` header validation. If you're expecting `Host` header validation, you can silence this warning by setting `Options.SilenceServersWarning = true`. See https://github.com/deepmap/oapi-codegen/issues/882 for more information.")
	}

	router, err := gorillamux.NewRouter(spec)
	if err != nil {
		panic(err)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if options == nil {
				performRequestValidationForErrorHandler(next, w, r, router, options, http.Error)
			} else if options.ErrorHandlerWithOpts != nil {
				performRequestValidationForErrorHandlerWithOpts(next, w, r, router, options)
			} else if options.ErrorHandler != nil {
				performRequestValidationForErrorHandler(next, w, r, router, options, options.ErrorHandler)
			} else {
				// NOTE that this shouldn't happen, but let's be sure that we always end up calling the default error handler if no other handler is defined
				performRequestValidationForErrorHandler(next, w, r, router, options, http.Error)
			}
		})
	}

}

func performRequestValidationForErrorHandler(next http.Handler, w http.ResponseWriter, r *http.Request, router routers.Router, options *Options, errorHandler ErrorHandler) {
	// validate request
	statusCode, err := validateRequest(r, router, options)
	if err == nil {
		// serve
		next.ServeHTTP(w, r)
		return
	}

	errorHandler(w, err.Error(), statusCode)
}

// Note that this is an inline-and-modified version of `validateRequest`, with a simplified control flow and providing full access to the `error` for the `ErrorHandlerWithOpts` function.
func performRequestValidationForErrorHandlerWithOpts(next http.Handler, w http.ResponseWriter, r *http.Request, router routers.Router, options *Options) {
	// Find route
	route, pathParams, err := router.FindRoute(r)
	if err != nil {
		errOpts := ErrorHandlerOpts{
			// MatchedRoute will be nil, as we've not matched a route we know about
			StatusCode: http.StatusNotFound,
		}

		options.ErrorHandlerWithOpts(r.Context(), err, w, r, errOpts)
		return
	}

	errOpts := ErrorHandlerOpts{
		MatchedRoute: &ErrorHandlerOptsMatchedRoute{
			Route:      route,
			PathParams: pathParams,
		},
		// other options will be added before executing
	}

	// Validate request
	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: pathParams,
		Route:      route,
	}

	if options != nil {
		requestValidationInput.Options = &options.Options
	}

	err = openapi3filter.ValidateRequest(r.Context(), requestValidationInput)
	if err == nil {
		// it's a valid request, so serve it
		next.ServeHTTP(w, r)
		return
	}

	var theErr error

	switch e := err.(type) {
	case openapi3.MultiError:
		theErr = e
		errOpts.StatusCode = determineStatusCodeForMultiError(e)
	case *openapi3filter.RequestError:
		// We've got a bad request
		theErr = e
		errOpts.StatusCode = http.StatusBadRequest
	case *openapi3filter.SecurityRequirementsError:
		theErr = e
		errOpts.StatusCode = http.StatusUnauthorized
	default:
		// This should never happen today, but if our upstream code changes,
		// we don't want to crash the server, so handle the unexpected error.
		// return http.StatusInternalServerError,
		theErr = fmt.Errorf("error validating route: %w", e)
		errOpts.StatusCode = http.StatusInternalServerError
	}

	options.ErrorHandlerWithOpts(r.Context(), theErr, w, r, errOpts)
}

// validateRequest is called from the middleware above and actually does the work
// of validating a request.
func validateRequest(r *http.Request, router routers.Router, options *Options) (int, error) {

	// Find route
	route, pathParams, err := router.FindRoute(r)
	if err != nil {
		if errors.Is(err, routers.ErrMethodNotAllowed) {
			return http.StatusMethodNotAllowed, err
		}

		return http.StatusNotFound, err // We failed to find a matching route for the request.
	}

	// Validate request
	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: pathParams,
		Route:      route,
	}

	if options != nil {
		requestValidationInput.Options = &options.Options
	}

	if err := openapi3filter.ValidateRequest(r.Context(), requestValidationInput); err != nil {
		me := openapi3.MultiError{}
		if errors.As(err, &me) {
			errFunc := getMultiErrorHandlerFromOptions(options)
			return errFunc(me)
		}

		switch e := err.(type) {
		case *openapi3filter.RequestError:
			// We've got a bad request
			// Split up the verbose error by lines and return the first one
			// openapi errors seem to be multi-line with a decent message on the first
			errorLines := strings.Split(e.Error(), "\n")
			return http.StatusBadRequest, fmt.Errorf(errorLines[0])
		case *openapi3filter.SecurityRequirementsError:
			return http.StatusUnauthorized, err
		default:
			// This should never happen today, but if our upstream code changes,
			// we don't want to crash the server, so handle the unexpected error.
			return http.StatusInternalServerError, fmt.Errorf("error validating route: %s", err.Error())
		}
	}

	return http.StatusOK, nil
}

// attempt to get the MultiErrorHandler from the options. If it is not set,
// return a default handler
func getMultiErrorHandlerFromOptions(options *Options) MultiErrorHandler {
	if options == nil {
		return defaultMultiErrorHandler
	}

	if options.MultiErrorHandler == nil {
		return defaultMultiErrorHandler
	}

	return options.MultiErrorHandler
}

// defaultMultiErrorHandler returns a StatusBadRequest (400) and a list
// of all the errors. This method is called if there are no other
// methods defined on the options.
func defaultMultiErrorHandler(me openapi3.MultiError) (int, error) {
	return http.StatusBadRequest, me
}

func determineStatusCodeForMultiError(errs openapi3.MultiError) int {
	numRequestErrors := 0
	numSecurityRequirementsErrors := 0

	for _, err := range errs {
		switch err.(type) {
		case *openapi3filter.RequestError:
			numRequestErrors++
		case *openapi3filter.SecurityRequirementsError:
			numSecurityRequirementsErrors++
		default:
			// if we have /any/ unknown error types, we should suggest returning an HTTP 500 Internal Server Error
			return http.StatusInternalServerError
		}
	}

	if numRequestErrors > 0 && numSecurityRequirementsErrors > 0 {
		return http.StatusInternalServerError
	}

	if numRequestErrors > 0 {
		return http.StatusBadRequest
	}

	if numSecurityRequirementsErrors > 0 {
		return http.StatusUnauthorized
	}

	// we shouldn't hit this, but to be safe, return an HTTP 500 Internal Server Error if we don't have any cases above
	return http.StatusInternalServerError
}
