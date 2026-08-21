# The binary is static, so it does not care what runtime image carries it.
# CGO_ENABLED=0 is what makes it so: the SQLite driver is pure Go and every
# image codec is either the standard library or golang.org/x/image, so nothing
# here needs a C toolchain or a shared library at runtime.
#
# The check below is kept because this went wrong once: an encoder that
# dlopened a system library built and ran fine on a developer machine that
# happened to have it, and could not start in the image that shipped.
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

# Refuse to ship a binary that needs a loader, so the runtime image below stays
# an implementation detail rather than a dependency. The ELF interpreter path is
# in the first kilobyte if there is one at all.
RUN if head -c 1024 /out/asset-service | grep -qa 'ld-musl\|ld-linux'; then \
        echo "asset-service is dynamically linked; it must not depend on the runtime image" >&2; \
        exit 1; \
    fi

# Alpine, for one reason: ffmpeg. Video renditions are transcoded, there is no
# Go H.264 encoder worth having, and a distroless image cannot carry a
# dynamically linked ffmpeg or the loader it needs. That trade is real -- this
# image has a shell and a package manager and the previous one had neither --
# and it is made deliberately, because the alternative is a service that stores
# a 40 MB camera file and serves it to phones.
#
# The binary above stays static regardless, so nothing here is load-bearing for
# the service itself and the runtime image can be swapped again later.
FROM alpine:3.22

RUN apk add --no-cache ffmpeg

# The title label is what release cleanup filters on, so an unused image can be
# removed without a prune that reaches anything else on the host.
LABEL org.opencontainers.image.title="asset-service"
LABEL org.opencontainers.image.source="https://github.com/basicallysource/asset-service"

COPY --from=build /out/asset-service /usr/local/bin/asset-service

# 65532 is what the distroless nonroot user was, and the data volume on an
# existing host is owned by it -- changing the number would make the catalog
# unwritable after an upgrade. Given numerically rather than by name because
# nothing here reads /etc/passwd, and a user that does not have to be created
# cannot be created wrongly.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/asset-service"]
CMD ["serve"]
