# Build from the repository root: docker build -f cmd/auth/Dockerfile -t hypeman-auth .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /hypeman-auth ./cmd/auth

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hypeman-auth /hypeman-auth
EXPOSE 8080
ENTRYPOINT ["/hypeman-auth"]
