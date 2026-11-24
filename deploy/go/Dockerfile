# Shared minimal builder for the gateway, router and egress proxy.
# Build from the repository root: docker build -f deploy/go/Dockerfile --build-arg PKG=cmd/router .
FROM golang:1.26-alpine AS build
ARG PKG=cmd/gateway
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /padi-bin "./${PKG}"

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /padi-bin /usr/local/bin/padi-bin
ENTRYPOINT ["/usr/local/bin/padi-bin"]
