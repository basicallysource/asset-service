# asset-service

Stores files, names each one after a hash of its own bytes, produces the
smaller copies a web page should actually download, and hands out URLs to them.

```
POST /v1/assets?namespace=docs&filename=diagram.png     upload, returns a manifest
GET  /v1/assets/{key}                                   the manifest
GET  /a/{key}                                           302 to the bytes
GET  /healthz  GET /readyz                              liveness, readiness
```

Uploading `diagram.png` to the `docs` namespace stores it at
`docs/diagram-3f7a91c2b04e.png`, where the hex is the start of the SHA-256 of
the file. That one decision is most of the design:

- **Nothing is ever overwritten.** Different bytes cannot land on the same key,
  so a URL always means one exact file and can be cached forever.
- **Uploads are idempotent.** Sending the same file twice is not an error and
  does not store it twice; the second call returns the asset that is already
  there.
- **A key cannot lie.** The service computes the hash itself. Callers propose a
  filename, never a key.

Upload the original and the web-friendly copies are built for it in the
background. An image gets a JPEG ladder, 320px through 2048px, never wider than
what was uploaded -- PNG where it really uses transparency. A video gets H.264 MP4 encodes and a still to show before it
plays. A 3D model (STL) gets a rendered PNG and two slice reports: the
print cost in grams and minutes, with and without supports, measured by
OrcaSlicer on a worker rather than estimated. The manifest lists what exists, how big the original is, and whether
more is coming, so a page can ask for the narrowest rung that suits it instead
of a forty-megabyte file off a camera.

The service does not serve bytes. It answers with a URL -- public for public
assets, signed and expiring for private ones -- and the reader fetches from
storage directly, so one download is one transfer out of storage rather than
two.

## Running it

```sh
go test ./...
go run ./cmd/asset-service serve
```

It needs an S3-compatible bucket and a few environment variables; see
[`deploy/asset-service.env.example`](deploy/asset-service.env.example) for the
full list, and [`agent-docs/operations.md`](agent-docs/operations.md) for
running it on a host.

Get a credential by signing in with GitHub -- nobody has to approve you, and
the account you sign in as gets a namespace of its own with limits attached:

```sh
asset-service login --url https://assets.example.com
asset-service upload diagram.png
```

There is a page at `/login` that does the same in a browser. On the host, an
operator can also mint one directly:

```sh
ASSET_DB_PATH=./catalog.db go run ./cmd/asset-service keys add ci write:docs
```

Use it:

```sh
curl -X POST --data-binary @diagram.png \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: image/png" \
  "http://localhost:8080/v1/assets?namespace=docs&filename=diagram.png"
```

## Documentation

- [`agent-docs/uploading.md`](agent-docs/uploading.md) -- getting a token and
  putting a file in, start to finish.
- [`agent-docs/architecture.md`](agent-docs/architecture.md) -- what is
  committed to and why, including what is deliberately not here yet.
- [`agent-docs/api.md`](agent-docs/api.md) -- every route, in detail.
- [`agent-docs/operations.md`](agent-docs/operations.md) -- configuration,
  deployment, releases.
