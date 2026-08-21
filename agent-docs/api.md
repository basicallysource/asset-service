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

Every response is `Cache-Control: no-store` except a delivery redirect. That
matters more than it looks: a key ends in the asset's own extension, so
`/v1/assets/docs/diagram-3f7a91c2b04e.png` looks like an image to a CDN, which
will otherwise cache it -- and a manifest that says renditions are still being
built would keep saying so long after they were done.

## Authentication

```
Authorization: Bearer asset_<id>_<secret>
```

A key holds scopes of the form `<action>:<namespace>` -- `write:docs`,
`read:*`, `admin:docs`. The actions are `read`, `write`, and `admin`, which is
the right to mint and revoke credentials for that namespace. There is no
hierarchy between them.

A request with no credential is anonymous, which is enough to read anything
public. A request with a credential that does not work is rejected immediately,
on any route.

To get a credential, see [uploading.md](uploading.md), or the routes below.

## POST /v1/auth/github/start

Begins a sign-in. No credential needed; rate limited per caller.

Returns `device_code`, `user_code`, `verification_uri`, `expires_in` and
`interval`. Show the person the code and the URL.

`501` means this service was started without a GitHub client id and issues no
credentials of its own.

## POST /v1/auth/github/token

Body: `{"device_code": "..."}`.

`202 {"status":"pending"}` means keep waiting; poll no faster than `interval`,
and slow down further on `{"status":"slow_down"}`. `201` returns the token, the
account it belongs to, the namespace it may write to, and the limits it is held
to. `400` means the code expired.

## POST /v1/keys

Mint a credential. Body: `{"name": "...", "scopes": ["write:docs"],
"expires_in_days": 90}`. Requires `admin` on every namespace named in `scopes`
-- a key can only ever grant what its creator already administers. The token is
in the response and nowhere else.

## GET /v1/keys

Lists the keys the caller could have created itself. Others are not shown.

## POST /v1/keys/{name}/revoke

Makes a key stop working, at once. A key the caller may not administer answers
`404`, the same as one that does not exist.

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
  "renditions_status": "ready",
  "renditions": [
    {"name": "w320", "content_type": "image/webp", "width": 320, "height": 240,
     "size": 18204, "url": "https://cdn.example.com/docs/diagram-w320-8c1d.webp"},
    {"name": "w640", "content_type": "image/webp", "width": 640, "height": 480,
     "size": 49118, "url": "https://cdn.example.com/docs/diagram-w640-2b7f.webp"},
    {"name": "original", "content_type": "image/png", "size": 48213,
     "url": "https://cdn.example.com/docs/diagram-3f7a91c2b04e.png"}
  ]
}
```

`renditions` is the ladder: every form of this asset that can be fetched,
smallest first, with the bytes as uploaded last. Walk it for the first rung
wide enough for where you are showing the image, and fall back to the last
entry. Read it rather than assuming what is in it -- an image too small to
shrink usefully has a ladder of one.

`renditions_status` says whether the ladder is finished:

| | |
|---|---|
| `ready` | Nothing more is coming. |
| `pending` | Derived forms are queued or being produced. Poll, or use what is there. |
| `failed` | Producing them was given up on. What is listed is all there will be. |
| `none` | This kind of asset has no derived forms -- an STL, a zip, a video. |

Images are queued the moment they are uploaded, so a manifest fetched straight
after an upload usually says `pending`. Widths are produced only below the
original's own width: nothing is ever upscaled.

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
