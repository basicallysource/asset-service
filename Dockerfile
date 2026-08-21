# The binary is static (CGO_ENABLED=0, and the SQLite driver is pure Go), so
# the runtime image needs nothing in it but the binary.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a code-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/asset-service ./cmd/asset-service

# Distroless static: no shell, no package manager, nothing to exploit that is
# not the service itself. Operator commands run as `docker exec <container>
# asset-service keys ...`, which needs no shell.
FROM gcr.io/distroless/static-debian12:nonroot

# The title label is what release cleanup filters on, so an unused image can be
# removed without a prune that reaches anything else on the host.
LABEL org.opencontainers.image.title="asset-service"
LABEL org.opencontainers.image.source="https://github.com/basicallysource/asset-service"

COPY --from=build /out/asset-service /usr/local/bin/asset-service

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/asset-service"]
CMD ["serve"]
