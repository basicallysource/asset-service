# Architecture

What this service is committed to, why, and what is deliberately still missing.
Anything not written down here is not settled; anything here should be changed
by editing this document in the same change that changes the code.

## The one idea

An asset is named after a hash of its own bytes:

```
<namespace>/<name>-<hash>.<ext>        docs/diagram-3f7a91c2b04e.png
```

The name is for humans reading a bucket listing; the hash is the part that
matters. Everything else follows from it:

- A URL is permanent and cacheable forever, because nothing else can ever be
  served under that name.
- Storing the same file twice is a no-op rather than a conflict.
- Publishing new bytes cannot break a reference to the old ones, so a caller
  never needs to be told to re-fetch or invalidate anything.
- The service computes the key. A caller proposes a namespace and a filename;
  it cannot name a key, so a name can never disagree with its content.

The one way this could go wrong is two different files sharing a hash prefix
under the same name. That is refused, loudly, in both the catalog and storage
(`ErrDigestCollision`) rather than resolved. It has never happened; the point is
that it cannot happen *quietly*.

## Committed decisions

**Go, one binary, no framework.** Routing is `net/http`'s own multiplexer;
middleware is `func(http.Handler) http.Handler`. The service is small enough
that a framework would be the largest thing in it.

**Storage is S3-compatible, and the client is written here.**
`internal/objstore` implements the three operations this service performs --
HEAD, PUT, and a query-signed GET -- over SigV4. An SDK would be a large
dependency tree for that. The signer is tested against signatures produced by an
independent implementation, and against a live provider when one is configured.

**The service hands out URLs; it does not carry bytes.** There is no proxy
path and no `Get` in the storage interface. A reader is redirected to the
object: to a public URL, or to a signed URL that expires. This is the difference
between one transfer out of storage per download and two, which is the whole
cost model of a service like this.

**SQLite is an index, not the truth.** Every fact about an asset -- content
type, size, and its digest as object metadata -- is also on the object in
storage. Losing the database costs the API key list and a listing pass over the
bucket. It cannot lose an asset. Nothing in the schema is mutable except an API
key's revocation, because assets are immutable by construction.

**Credentials are API keys with namespace scopes.** A scope is
`<action>:<namespace>`, action being `read` or `write`, namespace possibly `*`.
Keys are minted by an operator command against the database, never over HTTP.
Only a hash of each token's secret is stored.

**Anonymous requests are legitimate.** Public assets are readable without a
credential; a request that *presents* a credential that does not work is
rejected at the edge. Whether a private asset exists is itself private, so an
unauthorised read gets the same 404 a missing asset gets.

**Releases are automatic and versioned by derivation.** Merging to `main` runs
the tests, builds an image, then tags the next patch version and publishes a
release naming the image digest. Hosts poll for releases and install by digest.
Nothing is hand-tagged, and a release that exists is always one whose tests
passed and whose image is in the registry.

## Layout

```
cmd/asset-service      wiring, the operator CLI, graceful shutdown
internal/config        environment -> one validated struct, once
internal/httpx         middleware and the single way to write a response
internal/auth          who is calling, and what they may do
internal/objstore      S3-compatible storage: SigV4, a client, an in-memory double
internal/catalog       SQLite: assets and API keys, with migrations
internal/assets        the domain: hash, name, store, resolve
internal/api           routes and their access rules
deploy                 how a host runs and updates it
```

The dependency direction is one-way: `api` -> `assets` -> {`objstore`,
`catalog`}, with `httpx` and `config` as leaves. `auth` knows nothing about
assets, and `assets` knows nothing about HTTP.

## Not built yet, and where it goes

These are expected. Each names the seam it arrives at, so the first one does not
require rearranging the service.

**Renditions -- the size ladder.** A manifest already carries a `renditions`
array; today it holds one entry, the bytes as uploaded. Derived forms (an image
at several widths, a compressed variant, a poster frame) append to it. They need
a `renditions` table keyed by asset, a job that produces them, and a rule for
which are worth producing. A caller written against the manifest today keeps
working when they appear.

**A second kind of credential.** `auth.Authenticator` takes the whole request
and returns a `Principal`. A signed-in user, or a session minted by another
service, is a second implementation and a way to combine the two. No handler
changes: handlers only ask the principal what it may do.

**Per-asset permissions.** Scopes are namespace-wide, which is the right
granularity for machines. Anything finer -- this user may see these assets --
belongs in the principal's scopes or in a check beside the visibility check in
`readable`, not in a new concept.

**Large uploads.** A body is spooled to disk and stored in a single PUT, which
is fine to a few gigabytes and simple. Multipart upload belongs in `objstore`
behind the same `Put`.

**Deletion and garbage collection.** There is no delete, deliberately: an
immutable URL that stops working is worse than a stored file nobody references.
When storage costs enough to matter, the answer is a sweep that reports what is
unreferenced, not a delete verb on the API.

**Listing and inventory.** Rebuilding the catalog from a bucket listing is the
first real use, and it needs LIST in `objstore`.
