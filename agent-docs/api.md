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
| `namespace` | required | Lowercase letters, digits and dashes, and not one of this service's own route names (`v1`, `a`, `login`, `healthz`, `readyz`) -- those are refused so that assets and the API can share a hostname. The unit access is granted over. |
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
  "width": 1600,
  "height": 1200,
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

`width` and `height` are the original's own pixel size, present for the kinds
of asset that have one. Use them to reserve space before the bytes arrive --
`<img width height>` or an `aspect-ratio` box -- so the page does not jump when
they do. Take them from here rather than from a rendition: the ladder tops out
below what a camera produces, and an asset too small to shrink has no ladder to
read a shape from. Zero means the service could not measure it.

`renditions` is the ladder: every form of this asset that can be fetched,
smallest first, with the bytes as uploaded last. Walk it for the first rung
wide enough for where you are showing the image, and fall back to the last
entry. Read it rather than assuming what is in it -- an image too small to
shrink usefully has a ladder of one.

An image's rungs are all WebP. A video's are H.264 in MP4, plus one still named
`poster`. **Tell them apart by `content_type`, not by name**: the poster has a
width like any other rung, and treating it as a video would hand a browser an
image where it expected something to play.

`renditions_status` says whether the ladder is finished:

| | |
|---|---|
| `ready` | Nothing more is coming. |
| `pending` | Derived forms are queued or being produced. Poll, or use what is there. |
| `failed` | Producing them was given up on. What is listed is all there will be. |
| `none` | This kind of asset has no derived forms -- an STL, a zip, a PDF. |

Images and videos are queued the moment they are uploaded, so a manifest
fetched straight after an upload usually says `pending`. An image takes
seconds; a video is transcoded one at a time and takes as long as that takes.

Nothing is ever upscaled. An image's rungs stop below its own width, because a
WebP the same size as the original is not worth the bytes. A video's go up to
and including its own width, and a video narrower than every configured width
gets one encode at its own size, because for video the saving is the bitrate
rather than the pixel count -- a minute off a camera is tens of megabytes and
the same frames re-encoded are a tenth of that. Either way there is a poster,
so a page can show something without downloading a video at all.

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

## POST /v1/jobs/claim

Take the oldest job that is due. `200` returns the asset's key, content type
and a `source_url` to read the original from; `204` means there is nothing to
do. Requires `admin:*` -- a worker decides what an asset's smaller copies look
like, which is not something a write scope buys.

A claim is not a lease you renew. Go quiet for half an hour and the job is
offered to somebody else, because a worker on another machine can die without
anything here noticing.

## POST /v1/jobs/renditions

Store one derived form. Query: `key`, `name` (`w640`, `poster`), `width`,
`height`, `ext`. The body is the raw bytes and `Content-Type` is what they are.
Requires `admin` on the key's namespace.

The key is computed here from the bytes, exactly as it is for an upload: a
worker can no more name an object than an uploader can.

## POST /v1/jobs/finish

Close a claim. Query: `key`. Body, optional:
`{"error": "...", "permanent": true}`. No error means it worked. An error puts
the job back for another attempt unless `permanent` says the bytes will never
derive, in which case there is nothing to come back for.

## GET /healthz, GET /readyz

`healthz` says the process is up and answers without touching anything else.
It is what a supervisor should restart on.

`readyz` also checks that the catalog opens and that storage answers with the
credentials it was given, which is what a deploy and an external monitor should
watch: a process can be running while the storage behind it is unreachable, and
`healthz` would still say yes. Match on `"status":"ready"` rather than on the
status code alone.

The storage half of `readyz` is checked at most once every few seconds and the
answer is shared. The route is unauthenticated, because a monitor is the point
of it, and without that window a flood of cheap requests here would become a
flood of expensive ones against the object store.
