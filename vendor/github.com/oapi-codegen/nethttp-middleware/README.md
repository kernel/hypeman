# OpenAPI Validation Middleware for `net/http`-compatible servers

An HTTP middleware to perform validation of incoming requests via an OpenAPI specification.

This project is a lightweight wrapper over the excellent [kin-openapi](https://github.com/getkin/kin-openapi) library's [`openapi3filter` package](https://pkg.go.dev/github.com/getkin/kin-openapi/openapi3filter).

This is _intended_ to be used with code that's generated through [`oapi-codegen`](https://github.com/oapi-codegen/oapi-codegen), but should work otherwise.

⚠️ This README may be for the latest development version, which may contain unreleased changes. Please ensure you're looking at the README for the latest release version.

## Usage

You can add the middleware to your project with:

```sh
go get github.com/oapi-codegen/nethttp-middleware
```

There is a full example of usage in [the Go doc for this project](https://pkg.go.dev/github.com/oapi-codegen/nethttp-middleware#pkg-examples).

A simplified version of this code is as follows:

```go
rawSpec := `
openapi: "3.0.0"
# ...
`
spec, _ := openapi3.NewLoader().LoadFromData([]byte(rawSpec))

// NOTE that we need to make sure that the `Servers` aren't set, otherwise the OpenAPI validation middleware will validate that the `Host` header (of incoming requests) are targeting known `Servers` in the OpenAPI spec
// See also: Options#SilenceServersWarning
spec.Servers = nil

router := http.NewServeMux()

router.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
    fmt.Printf("%s /resource was called\n", r.Method)

    if r.Method == http.MethodPost {
        w.WriteHeader(http.StatusNoContent)
        return
    }

    w.WriteHeader(http.StatusMethodNotAllowed)
})

use := func(r *http.ServeMux, middlewares ...func(next http.Handler) http.Handler) http.Handler {
    var s http.Handler
    s = r

    for _, mw := range middlewares {
        s = mw(s)
    }

    return s
}

// create middleware
mw := middleware.OapiRequestValidatorWithOptions(spec, &middleware.Options{
    Options: openapi3filter.Options{
        AuthenticationFunc: authenticationFunc,
    },
})

// then wire it in
server := use(router, mw)

// now all HTTP routes will be handled by the middleware, and any requests that are invalid will be rejected
```

## FAQs

### Which HTTP servers should this work with?

If you're using something that's compliant with `net/http` (which should be all Go web frameworks / routers / HTTP servers) it should work as-is.

We explicitly test with the following servers, as they correspond with versions used by users of [oapi-codegen/oapi-codegen](https://github.com/oapi-codegen/oapi-codegen):

- [Chi](https://github.com/go-chi/chi)
- [gorilla/mux](https://github.com/gorilla/mux)
- [net/http](https://pkg.go.dev/net/http)

### "This doesn't support ..." / "I think it's a bug that ..."

As this project is a lightweight wrapper over [kin-openapi](https://github.com/getkin/kin-openapi)'s [`openapi3filter` package](https://pkg.go.dev/github.com/getkin/kin-openapi/openapi3filter), it's _likely_ that any bugs/features are better sent upstream.

However, it's worth raising an issue here instead, as it'll allow us to triage it before it goes to the kin-openapi maintainers.

Additionally, as `oapi-codegen` contains [a number of middleware modules](https://github.com/search?q=org%3Aoapi-codegen+middleware&type=repositories), we'll very likely want to implement the same functionality across all the middlewares, so it may take a bit more coordination to get the changes in across our middlewares.

### I've just updated my version of `kin-openapi`, and now I can't build my code 😠

The [kin-openapi](https://github.com/getkin/kin-openapi) project - which we 💜 for providing a great library and set of tooling for interacting with OpenAPI - is a pre-v1 release, which means that they're within their rights to push breaking changes.

This may lead to breakage in your consuming code, and if so, sorry that's happened!

We'll be aware of the issue, and will work to update both the core `oapi-codegen` and the middlewares accordingly.
