# API

Five routes. Every response is JSON except a delivery redirect, which has no
body at all.

Failures share one shape:

```json
{"error": {"code": "not_found", "message": "no such asset"}}
```

`code` is stable and meant to be branched on: `bad_request`, `unauthorized`,
`forbidden`, `not_found`, `conflict`, `too_large`, `internal`.

Every response carries `X-Request-Id`. Send your own and it is reused, so one
id can follow a request across services.

## Authentication

```
Authorization: Bearer asset_<id>_<secret>
```

A key holds scopes of the form `<action>:<namespace>` -- `write:docs`,
`read:*`. A request with no credential is anonymous, which is enough to read
anything public. A request with a credential that does not work is rejected
immediately, on any route.

## POST /v1/assets

Upload. The body is the raw file; there is no multipart form.

| Parameter | | |
|---|---|---|
| `namespace` | required | Lowercase letters, digits and dashes. The unit access is granted over. |
| `filename` | required | The original name. Used for the readable half of the key and kept in the manifest. |
| `visibility` | optional | `public` (default) or `private`. |

`Content-Type` on the request becomes the asset's content type. If it is absent
or `application/octet-stream`, it is inferred from the extension.

Requires `write:<namespace>`.

```sh
curl -X POST --data-binary @diagram.png \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: image/png" \
  "https://assets.example.com/v1/assets?namespace=docs&filename=diagram.png"
```

`201` when this call stored the asset, `200` when it was already there -- the
same manifest either way, and `Location` names the asset. Uploading the same
bytes again is not an error and stores nothing new.

Other answers: `400` unusable namespace, filename, visibility or empty body;
`401` no credential; `403` no write scope for that namespace; `409` the key
already holds different bytes (a hash prefix collision, which is refused rather
than resolved); `413` past the configured size limit.

## GET /v1/assets/{key}

The manifest. Public assets need no credential; private ones need
`read:<namespace>`.

```json
{
  "key": "docs/diagram-3f7a91c2b04e.png",
  "namespace": "docs",
  "digest": "sha256:3f7a91c2b04e...",
  "size": 48213,
  "content_type": "image/png",
  "filename": "diagram.png",
  "visibility": "public",
  "created_at": "2026-08-21T04:12:07Z",
  "url": "https://cdn.example.com/docs/diagram-3f7a91c2b04e.png",
  "url_expires": false,
  "renditions": [
    {"name": "original", "content_type": "image/png", "size": 48213,
     "url": "https://cdn.example.com/docs/diagram-3f7a91c2b04e.png"}
  ]
}
```

`renditions` is the ladder: every form of this asset that can be fetched. It
holds one entry today. Derived forms are appended as they are produced, so read
the ladder and pick rather than assuming what is in it.

`url_expires` says whether `url` is stable or time-limited. A stable URL can be
kept forever. An expiring one must be fetched again, not cached.

## GET /a/{key}

`302` to the bytes, with no body. Public assets redirect to a durable URL and
may be cached for a day; private assets redirect to a signed URL and are
`private, no-store`.

Use this when you want one permanent address to hand around. Use the `url` from
a manifest when you can, since it skips the extra round trip.

A private asset that the caller may not read answers `404`, the same as one that
does not exist.

## GET /healthz, GET /readyz

`healthz` says the process is up and answers without touching anything else.
`readyz` also checks that the catalog opens and that storage answers with the
credentials it was given; it is what a deploy should gate on.
