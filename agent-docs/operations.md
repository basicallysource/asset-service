# Operations

## Configuration

Environment only, read once at startup. A missing or malformed variable is
reported at startup with every other problem in the same message, rather than
one restart at a time.

| Variable | Default | |
|---|---|---|
| `ASSET_DB_PATH` | required | SQLite catalog. Its directory is created if missing. |
| `ASSET_PUBLIC_BASE_URL` | required | Where readers fetch public objects: a CDN or other edge in front of the bucket. |
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

## Keys

Minting a credential is an operator action on the host, not a route.

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
Old images of this service are pruned after a week; nothing else on the host is
touched.

Two files configure a deployment, deliberately separate. `/opt/asset-service/.env`
holds only which image to run and belongs to the release agent, which rewrites
it on every install. `/etc/asset-service/asset-service.env` holds how the
service runs and belongs to the operator; a release never reads or changes it.

The compose file binds the service to loopback and expects something on the host
to publish it. Traefik labels are included and are inert unless
`ASSET_SERVICE_TRAEFIK=true`.

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
