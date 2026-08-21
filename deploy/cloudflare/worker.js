// Serve the assets from the same hostname as the API.
//
// Without this, a public URL names the bucket: something like
// `<bucket>.<region>.cdn.<provider>.com/<namespace>/<file>`. That works, and it
// is what the service hands out by default, but it puts a provider's hostname
// in every page that shows an image and makes moving providers a rewrite of
// every URL ever published.
//
// This puts them on your own hostname instead. It is a pass-through, not a
// store: a request for an asset is fetched from the CDN in front of the bucket
// and returned, and everything else goes to the service as before. Which is
// which is decided by the first path segment -- the service refuses its own
// route names as namespaces, so the two can never mean the same thing.
//
// The bytes are immutable (a key contains a hash of the content it names), so
// the edge may keep them forever and a hit never reaches the bucket at all.
// That is the point: it is the difference between paying a provider for every
// download and paying for the first one.
//
// Deploy: set ASSET_ORIGIN in wrangler.toml to the CDN in front of the bucket,
// route the Worker at your asset hostname, and set the service's
// ASSET_PUBLIC_BASE_URL to that hostname.

// The service's own top-level routes. Kept in step with ReservedNamespaces in
// internal/assets/key.go, which refuses these as namespaces.
const SERVICE_ROUTES = new Set(['v1', 'a', 'login', 'healthz', 'readyz']);

// A year. Safe because a key names its own content: these bytes cannot change.
const IMMUTABLE = 'public, max-age=31536000, immutable';

export default {
	async fetch(request, env, ctx) {
		const url = new URL(request.url);
		const segments = url.pathname.split('/').filter(Boolean);

		const isAsset =
			(request.method === 'GET' || request.method === 'HEAD') &&
			segments.length === 2 &&
			!SERVICE_ROUTES.has(segments[0]);

		// Anything that is not an asset is the service's: the API, the sign-in
		// page, the health checks. Fetching the request as it stands reaches
		// the origin without coming back through this Worker.
		if (!isAsset) return fetch(request);

		const target = new URL(url.pathname, env.ASSET_ORIGIN);
		const response = await fetch(target, {
			method: request.method,
			headers: forwarded(request.headers),
			cf: { cacheEverything: true, cacheTtl: 31536000 },
		});

		// Rewrite rather than pass through, so a bucket that was configured
		// with a shorter TTL does not decide how long browsers keep something
		// that can never change.
		const headers = new Headers(response.headers);
		if (response.ok) headers.set('Cache-Control', IMMUTABLE);
		headers.delete('x-amz-request-id');
		headers.delete('x-amz-id-2');
		headers.set('Access-Control-Allow-Origin', '*');

		return new Response(response.body, {
			status: response.status,
			statusText: response.statusText,
			headers,
		});
	},
};

// forwarded keeps only the headers that change what comes back. Everything
// else -- cookies above all -- has no business reaching a public bucket.
function forwarded(headers) {
	const out = new Headers();
	for (const name of ['accept', 'accept-encoding', 'range', 'if-none-match', 'if-modified-since']) {
		const value = headers.get(name);
		if (value) out.set(name, value);
	}
	return out;
}
