# Operations

## Configuration

Environment only, read once at startup. A missing or malformed variable is
reported at startup with every other problem in the same message, rather than
one restart at a time.

| Variable | Default | |
|---|---|---|
| `ASSET_DB_PATH` | required | SQLite catalog. Its directory is created if missing. |
| `ASSET_PUBLIC_BASE_URL` | required | Where readers fetch public objects: a CDN or other edge in front of the bucket. To serve them from this service's own hostname instead of the bucket's, see [`deploy/cloudflare`](../deploy/cloudflare/README.md). |
| `ASSET_S3_ENDPOINT` | required | Regional endpoint, bucket excluded. |
| `ASSET_S3_REGION` | required | |
| `ASSET_S3_BUCKET` | required | |
| `ASSET_S3_ACCESS_KEY` | required | Scope it to this bucket alone. |
| `ASSET_S3_SECRET_KEY` | required | |
| `ASSET_S3_PATH_STYLE` | `false` | Address the bucket as a path segment. Needed by some providers. |
| `ASSET_ADDR` | `:8080` | |
| `ASSET_LOG_LEVEL` | `info` | |
| `ASSET_SPOOL_DIR` | system temp | Where a body is held while it is hashed. |
| `ASSET_MAX_UPLOAD_BYTES` | `256MiB` | Accepts `1048576` or `256MiB`. |
| `ASSET_UPLOAD_TIMEOUT` | `30m` | How long a body may take to arrive. |
| `ASSET_SIGNED_URL_TTL` | `15m` | Lifetime of a private asset's URL. |
| `ASSET_RENDITIONS` | `true` | Run the rendition worker in this process. Off still queues the work; see below. |
| `ASSET_RENDITION_WIDTHS` | `320,640,1024,1600,2048` | Widths to produce, below the original's own width. |
| `ASSET_RENDITION_QUALITY` | `82` | JPEG quality, 1-100. Does not apply to renditions kept as PNG. |
| `ASSET_RENDITION_POLL` | `15s` | Idle interval. Work normally starts at once; this catches anything missed. |
| `ASSET_RENDITION_ATTEMPTS` | `4` | Failures before an asset is left alone and reported as failed. |
| `ASSET_VIDEO_WIDTHS` | `960,1920` | Widths to encode video at, up to the source's own. A narrower source gets one encode at its own width. |
| `ASSET_VIDEO_CRF` | `26` | H.264 quality, 0-51, lower being better and larger. |
| `ASSET_VIDEO_PRESET` | `medium` | libx264 speed against size. `veryfast` on a busy host; `slow` if the bytes matter more than the wait. |
| `ASSET_GITHUB_CLIENT_ID` | none | Enables sign-in. A device-flow client id, which is public by design -- there is no secret. Empty means this service issues no credentials and an operator mints them on the host. |
| `ASSET_ADMIN_GITHUB_LOGINS` | none | Comma-separated GitHub logins that get full rights when they sign in. This is how the people who run it bootstrap without shell access. |
| `ASSET_CLIENT_IP_HEADER` | none | The header a proxy in front of this service sets to the real client address, e.g. `CF-Connecting-IP`. Empty trusts none, which is the only safe default -- any caller can send a header. |

Rendition work runs in the service's own process, one asset at a time. On a
host shared with something else, that is the thing that matters: resizing and
transcoding will both use everything they are given, and one worker keeps them
a good neighbour. A 2048px ladder off a 24-megapixel photograph takes a few
seconds of one core. A minute of 4K video takes minutes of one core -- ffmpeg
is pinned to a single thread on purpose, so a busy queue costs one core and not
the machine. `ASSET_VIDEO_PRESET=veryfast` trades perhaps a third more bytes
for a small fraction of the time.

**Video needs ffmpeg and ffprobe on `PATH`.** The image ships with them. Where
they are missing the service still runs and video simply has no derived forms,
which is what it did before it could transcode.

## Deriving on another machine

Transcoding will take a small host down with it, and the host a service like
this runs on is usually shared with something that matters more. So the work
does not have to happen there.

Set `ASSET_RENDITIONS=false` on the service. Uploads are still queued; nothing
in this process works the queue. Then, on any machine with cores to spare, with
a credential that has `admin:*` and ffmpeg installed:

```sh
ASSET_SERVICE_URL=https://assets.example.com ASSET_SERVICE_TOKEN=asset_... \
  asset-service work
```

It claims a job, fetches the original straight from storage, derives, sends the
bytes back, and asks for the next one. `--once` drains the queue and stops,
which is what a cron job or a burst machine wants; `--preset veryfast` trades
size for time. Stopping it at any point is safe: a claim that goes quiet for
half an hour is offered to somebody else.

Several workers can run at once. Each claim is taken under a write lock, so two
workers never get the same job.

## Measuring assets stored before dimensions existed

A manifest reports the original's pixel size, measured at upload. Assets stored
before that was true have zeros. On the host, once:

```sh
asset-service measure
```

It reads each unmeasured asset back out of storage, measures it, and records
it. It is bounded per run, safe to repeat, and touches nothing that already has
dimensions.

## Building derived forms for what is already stored

When the service learns to derive something it could not before -- video, say,
which stored fine long before anything could transcode it -- the assets already
in the bucket have no ladder and no job to build one. On the host:

```sh
asset-service requeue
```

It queues every asset that has no derived forms and is not already waiting for
some, and skips anything with a content type it still cannot do. Bounded per
run, safe to repeat. The running service picks the work up within a poll.

`asset-service requeue --rebuild` is the other case: what a rendition should
look like has changed -- a different format, different widths -- and the ones
already made are the old answer. It throws their rows away and makes them
again. The old objects stay in storage, unreferenced; a key names its bytes, so
nothing already pointing at one breaks.

## Withholding camera originals stored before the service did

An image or a video uploaded now is stored private and published as a copy
without the camera's own notes in it -- position, capture time, device. One
stored earlier is still an object anybody can fetch. On the host, one namespace
at a time:

```sh
asset-service withhold docs
```

Each asset goes one of two ways. Where there is already something to publish in
its place -- an image's `full` copy, a video's encodes -- the object's ACL is
rewritten and nothing else happens: no bytes are read or moved, so this costs
one request per asset whatever they weigh. Where there is not, the asset is
queued so the copy gets built, and a later run withholds it. Bounded per run
and safe to repeat; the usual course is to run it, wait for the queue, and run
it again.

Two things it does not do, both of which matter more than the command:

- **It does not reach a cache.** An object that has been public may sit in a
  CDN or a browser for as long as its cache headers said -- a year, under the
  immutable caching public objects are stored with. Withholding it stops new
  fetches from storage; it does not retract what has already been handed out.
  Where that matters, purge the CDN as well.
- **It does not rewrite pages.** Anything that published an original's URL
  starts getting a refusal. Rebuild those pages from the manifest -- `url` is
  the published form -- before withholding the namespace they point at, or
  accept that they break until you do.

## Sign-in

To let people get their own credentials, create a GitHub OAuth app with the
device flow enabled and set `ASSET_GITHUB_CLIENT_ID` to its client id. There is
no client secret to store: a device-flow client is a public one.

Whoever signs in gets a token confined to a namespace named after their
account, with the limits in `internal/policy` for their tier. Put your own
GitHub login in `ASSET_ADMIN_GITHUB_LOGINS` and signing in gives you a token
that can manage keys, so running the service needs no shell on the host.

There is a page at `/login` that does all of this in a browser and can mint
scoped keys.

## Accounts

```sh
asset-service accounts list             # who has signed in, and what they have used
asset-service accounts promote <handle> # contributor: 5x limits, any content type
asset-service accounts admin <handle>   # let them manage keys
asset-service accounts block <handle>   # stop them, and revoke their keys
asset-service accounts reset <handle>   # back to the default limits
```

Tiers decide limits: file size, uploads an hour, bytes a day, how many live
tokens, and which content types. An unknown account -- anyone who has signed in
-- may upload images, up to sizes and rates nobody working normally will reach.

## Keys

On the host, `keys` works against the database directly, which is how the first
credential exists before there is a service to ask. Everywhere else the same
commands run against the service you signed in to.

```sh
asset-service keys add ci-docs write:docs      # prints the token once
asset-service keys list
asset-service keys revoke ci-docs
```

The token is shown exactly once; only a hash of its secret is stored. Give each
caller its own key with the narrowest scope that works -- a build that publishes
documentation images wants `write:docs`, not `write:*`. Revocation takes effect
on the next request.

In a container: `docker exec asset-service-asset-service-1 asset-service keys
add ci-docs write:docs`.

## Deploying

The host pulls; nothing pushes to the host. That way shipping does not depend on
anything being able to reach it, and the host needs no inbound access.

```sh
install -d /opt/asset-service /var/lib/asset-service /etc/asset-service
install -m 0755 deploy/release-agent.sh /opt/asset-service/
install -m 0644 deploy/docker-compose.yml /opt/asset-service/
install -m 0600 deploy/asset-service.env.example /etc/asset-service/asset-service.env
$EDITOR /etc/asset-service/asset-service.env

# The image runs as uid 65532 and owns nothing it was not given.
chown -R 65532:65532 /var/lib/asset-service

install -m 0644 deploy/systemd/asset-service-release.* /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now asset-service-release.timer
```

The timer runs the agent every minute. It installs a release only when the
published version differs from what is recorded, pulls by digest, restarts,
waits for `/readyz`, and rolls back to the previous image if that never comes.
A release that fails is recorded in `installed-release.failed` and not tried
again until a newer one is published -- otherwise one bad build becomes an
outage on a loop, reinstalling and rolling back every minute. To retry one
deliberately, delete that file. Old images of this service are pruned after a
week; nothing else on the host is touched.

Two files configure a deployment, deliberately separate. `/opt/asset-service/.env`
holds only which image to run and belongs to the release agent, which rewrites
it on every install. `/etc/asset-service/asset-service.env` holds how the
service runs and belongs to the operator; a release never reads or changes it.

## Reaching it

The compose file binds the service to loopback and expects something on the host
to publish it. Set `ASSET_SERVICE_BIND` in `/opt/asset-service/.env` to a
private interface address to reach it from a private network instead, or to
`0.0.0.0` only when this service is the thing facing the internet.

Note what does and does not need to be reachable. Uploads and manifest lookups
go to this service; the bytes do not, since a reader is redirected to storage.
A site that resolves its URLs at build time never calls this service at
runtime, so a private address is often all it needs.

Behind Traefik with the file provider, route to the container by name and put
it on the proxy's network with a local `docker-compose.override.yml`:

```yaml
services:
  asset-service:
    container_name: asset-service
    networks: [web]
networks:
  web:
    external: true
```

```yaml
# in the proxy's dynamic configuration
http:
  routers:
    assets:
      rule: "Host(`assets.example.com`)"
      entryPoints: [websecure]
      service: assets
      tls: {certResolver: your-resolver}
  services:
    assets:
      loadBalancer:
        servers: [{url: "http://asset-service:8080"}]
```

With a label-driven proxy instead, add its labels in the same override file.

One thing to check before putting this behind a CDN or proxy: upload bodies can
be large, and several proxies cap request bodies well below this service's
limit. Cloudflare's free plan stops at 100 MB, for instance. Either raise the
proxy's cap, lower `ASSET_MAX_UPLOAD_BYTES` to match, or point uploads at the
origin.

## Releases

Merging to `main` runs the tests, builds the image, then tags the next patch
version and publishes a release carrying a `release.json` naming the image and
its digest. Version numbers are derived from the highest existing tag; nothing
is chosen by hand.

To ship without a merge, run the Release workflow manually. To hold a release
back, do not merge -- there is no other switch, on purpose.

## Backups and recovery

Back up `/var/lib/asset-service/catalog.db` (with its `-wal`) if you want key
management to survive a host loss. Assets do not depend on it: bytes, content
types and digests all live on the objects themselves, so a lost catalog costs
the key list and a listing pass over the bucket, not an asset.

Never restore a catalog over a bucket it does not belong to. Keys are derived
from content, so a mismatched pair announces itself as a digest conflict rather
than serving the wrong bytes -- but it is still a mess to unpick.

## Storage

Give the service a bucket of its own and a credential scoped to it. The service
is the only writer; content addressing means it never overwrites, but a
credential that *can* overwrite is a credential that can undo that guarantee,
so it should not be shared with anything else.

Public objects are stored `public-read` with a year of immutable caching, which
is safe because a key names its own bytes. Private objects are stored `private`
and reachable only through a signed URL, which is signed against the storage
endpoint rather than the CDN -- a signature covers the host it was made for.
