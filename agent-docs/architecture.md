# Architecture

What this service is committed to, why, and what is deliberately still missing.
Anything not written down here is not settled; anything here should be changed
by editing this document in the same change that changes the code.

## What it does

Takes a file, stores it once under a name derived from its own bytes, produces
the smaller copies a web page should actually download, and hands out URLs.

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
`<action>:<namespace>`; the action is `read`, `write`, or `admin` -- the right
to hand out credentials for that namespace. Only a hash of each token's secret
is stored. A principal can only ever mint what it already administers, so no
chain of key creation widens access, and there is no hierarchy between actions:
`admin` does not imply `write`.

**Anyone can get a credential by proving who they are.** GitHub's device flow,
which suits a terminal or an agent in one: no redirect to catch, no client
secret, and the token GitHub returns is used once to ask who it belongs to and
then dropped. A self-served token is confined to a namespace named after the
account and expires.

**Limits belong to accounts, not tokens.** Minting another token must not buy
more capacity, or every limit is decoration. An account's tier -- unknown,
trusted, admin, blocked -- decides the size of a file, uploads an hour, bytes a
day, how many live tokens it may hold, and which content types it may store.
Unknown accounts upload images and nothing else, which is what contributors
actually need and what stops this becoming a way to hand out executables from a
domain that looks like ours. The numbers live in `internal/policy`, in one
place, so the rules can be read rather than inferred from handlers.

The whole point of those limits is to make an open door safe: they are set
where ordinary work never meets them and abuse meets them immediately.

**Anonymous requests are legitimate.** Public assets are readable without a
credential; a request that *presents* a credential that does not work is
rejected at the edge. Whether a private asset exists is itself private, so an
unauthorised read gets the same 404 a missing asset gets.

**Derived forms are produced in the background, never during an upload.**
Re-encoding a large photograph takes seconds and transcoding a video takes
minutes, and an upload that waited for that would time out on a slow connection
for a reason unrelated to the upload. The asset is queued, a single worker
builds its ladder, and the manifest says whether that has finished. One worker
at a time is deliberate: this usually runs beside other services on a small
machine, and both resizing and transcoding will take every core they are
offered. Encoding is pinned to one thread for the same reason.

**Derived images are JPEG, or PNG when the pixels need it.** WebP is roughly a
quarter smaller and that difference is real, but a page's images are things
people take away: right-click, save, open in something. A file a good number of
tools still refuse is a worse answer than a slightly larger one everything
reads. Transparency is asked of the pixels rather than the format, because a
PNG saved with an alpha channel it never uses is the common case; where it is
really used the rendition stays a PNG, since flattening onto a guessed
background is visibly wrong on any page with a dark mode.

This is also why nothing here links against anything: JPEG and PNG are the
standard library, and every decoder is pure Go. The encoder that was not cost
an outage.

**Video is stored as it arrives and served as something a browser should
download.** The upload is the camera's own file; the ladder is H.264 in MP4 at
a couple of widths, plus a still from a second in so a page can show something
before anyone presses play. A client tells the still from the encodes by
content type. This is the one place the service depends on a program it did not
write: there is no Go H.264 encoder worth having, so `internal/video` drives
ffmpeg, and the runtime image is Alpine with ffmpeg in it rather than a
distroless image with nothing in it. That trade is real and was made on
purpose. Where ffmpeg is absent the service still runs and video simply has no
derived forms, which is what it did before.

**A model gets a picture and the slicer's own numbers.** An STL's ladder is a
rendered PNG plus two JSON reports: what printing it costs with supports off
and with supports on, sliced under one fixed, named profile so any two reports
are ever comparable. The numbers are OrcaSlicer's own, read back out of the
3MF it exported, never an estimate. The render is pure Go and works anywhere;
slicing, like video, drives a program this service did not write -- with one
deliberate difference: OrcaSlicer is not in the serving image, so
`model.Supported` asks about the content type alone. The box that stores an
upload queues the job without asking what is installed on it, and a worker
that has a slicer (`ORCA_BIN`, `ORCA_PROFILES`) claims it. Where none does,
the job fails visibly rather than completing with the numbers missing. Both
support variants are sliced every time because grams turn on that one boolean;
a fixed pair of renditions is what keeps the queue and the manifest free of
parameters.

**Deriving can happen on a different machine.** The queue, the bytes and every
naming decision stay here; the CPU time does not have to. `POST /v1/jobs/claim`
hands out a job and a URL to the original, the worker sends back what it made,
and `asset-service work` is that worker. This exists because the box a small
service runs on is chosen for being cheap and shared, and transcoding is the
one thing in here that will take a machine down with it -- so the box that
serves assets must never have to be the box that transcodes them. A worker in
this process and a worker on another continent go through the same
`Service.PutRendition`, so where the bytes were made cannot change how they are
stored or named.

**One place decides what has derived forms.** `internal/derive` dispatches on
content type for both questions -- may this be queued, and how is it made --
so the upload path and the worker cannot disagree about what the service can
do. A new kind of asset is a backend there and nothing else.

**A manifest says how big the original is.** Width and height of the bytes as
uploaded, measured once at upload and stored beside them. It is what lets a
page reserve the right space before an image arrives, which is the difference
between a page that settles and one that jumps. Deriving it from a rendition is
not the same answer: the ladder tops out below what a camera produces, and an
asset too small to shrink has no ladder to read a shape from at all. This is
the only thing about a stored asset that is ever written after the fact, and
only because it is a property of bytes that never change -- `measure` records
it for assets stored before the service could.

**Finished work leaves a record.** The job queue forgets a row the moment it
is done -- that is what makes it a queue -- so CompleteJob and FailJob append
one line to a `derivations` table on the way out: asset, content type,
outcome, attempt count, and claim-to-finish seconds. Append-only and a few
tiny rows per day, it is the answer to "how often do we derive, how long does
slicing take, what keeps failing" (`asset-service stats`, on the host) without
a metrics stack. Logging is best-effort by design: a log insert must never
stop the queue.

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
internal/catalog       SQLite: assets, renditions, the job queue, API keys
internal/imaging       bytes in, smaller bytes out -- no storage, no database
internal/video         the same for video, by driving ffmpeg
internal/model         renders and slice reports for 3D models, via OrcaSlicer
internal/derive        which of those applies, and the one place that decides
internal/identity      proving who somebody is, over GitHub's device flow
internal/policy        what an account may do, in numbers, in one place
internal/assets        the domain: hash, name, store, resolve
internal/renditions    the worker that drains the queue
internal/api           routes, their access rules, and the sign-in page
deploy                 how a host runs and updates it
```

The dependency direction is one-way: `api` -> `assets` -> {`objstore`,
`catalog`}, with `httpx`, `config` and `derive` as leaves. `auth` knows nothing
about assets, `assets` knows nothing about HTTP, and `imaging` and `video` know
nothing about anything but bytes and files -- which is why the expensive part of
this service is testable without any of the rest of it.

## Not built yet, and where it goes

These are expected. Each names the seam it arrives at, so the first one does not
require rearranging the service.

**More kinds of derived form.** A PDF wants a first-page thumbnail; a zip
wants a listing. Each is a backend in `internal/derive` behind the same queue,
table and manifest, the way images, video and models already are.

**More identity providers.** `auth.Authenticator` takes the whole request and
returns a `Principal`, and `internal/identity` proves who somebody is. A second
provider -- a session minted by another service, an OIDC token from a build --
is a new implementation of each, not a change to any handler.

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
