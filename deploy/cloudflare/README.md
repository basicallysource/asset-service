# Serving assets from your own hostname

By default a public URL names the bucket:

    https://<bucket>.<region>.cdn.<provider>.com/web/diagram-3f7a91c2b04e.jpg

That works. It also puts a provider's hostname into every page that shows an
image, and makes changing providers a rewrite of every URL ever published.

`worker.js` is a Cloudflare Worker that puts them on the same hostname as the
service instead:

    https://assets.example.com/web/diagram-3f7a91c2b04e.jpg

It decides by the first path segment: `v1`, `a`, `login`, `healthz` and `readyz`
are the service's, everything else is an asset and is fetched from the CDN in
front of the bucket. The service refuses those five as namespaces
(`assets.ReservedNamespaces`), so the two can never come to mean the same thing.

A key contains a hash of the bytes it names, so nothing served here can change.
The Worker says `immutable` for a year and lets the edge keep it, which means a
download that hits cache never reaches the bucket at all -- the difference
between paying for every download and paying for the first one.

## Setting it up

1. Put the CDN hostname in `ASSET_ORIGIN` and your asset hostname in the route.
2. `wrangler deploy`
3. Set `ASSET_PUBLIC_BASE_URL` to the same hostname and restart the service.

Existing URLs keep working: the bucket's own hostname is still there, and a key
means the same bytes wherever it is read from.

## Without Cloudflare

Any CDN that can route by path prefix does this; the Worker is thirty lines
because the rule is "two segments and not one of five names". Or skip it and
give the bucket its own subdomain -- most providers support a custom hostname
on their CDN, which costs nothing to run but leaves the service on a different
name.
