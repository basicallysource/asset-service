# The binary is static, so the runtime image needs nothing in it but the
# binary. Two things make it static and both are load-bearing:
#
#   CGO_ENABLED=0    the SQLite driver is pure Go, so nothing needs a C
#                    toolchain.
#   -tags nodynamic  the WebP encoder embeds libwebp as WebAssembly instead of
#                    dlopen-ing the system copy. Without this tag it links
#                    dynamically and expects a libwebp on the host, which a
#                    distroless image does not have -- and neither does the
#                    loader it would need to find out. It builds and runs fine
#                    on a developer machine that happens to have libwebp,
#                    which is exactly how it reached production once.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a code-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -tags nodynamic \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/asset-service ./cmd/asset-service

# Refuse to ship a binary that needs a loader the runtime image does not have.
# The ELF interpreter path is in the first kilobyte if there is one at all.
RUN if head -c 1024 /out/asset-service | grep -qa 'ld-musl\|ld-linux'; then \
        echo "asset-service is dynamically linked; it will not start in a distroless image" >&2; \
        exit 1; \
    fi

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
