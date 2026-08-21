package objstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4, the only authentication S3-compatible storage
// accepts. It is implemented here rather than pulled from an SDK because the
// surface this service needs is three requests -- HEAD, PUT, and a query-signed
// GET -- and the signing algorithm is a page of pure functions with published
// test vectors. The SDK would be a large dependency tree in a public repo for
// no behaviour we use.
//
// The signer is exercised in sigv4_test.go against signatures produced by an
// independent implementation (botocore), which is what makes it trustworthy.

const (
	algorithm       = "AWS4-HMAC-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	stampFormat     = "20060102T150405Z"
	dateFormat      = "20060102"
	terminator      = "aws4_request"
)

type credentials struct {
	AccessKey string
	SecretKey string
}

// signRequest applies SigV4 header authentication to req in place.
//
// payloadHash is the hex SHA-256 of the body. This service always knows it
// before it sends: the hash that names an asset is the same hash S3 wants, so
// nothing is buffered or read twice to produce it.
func signRequest(req *http.Request, cr credentials, region, service, payloadHash string, now time.Time) {
	stamp := now.UTC().Format(stampFormat)
	date := now.UTC().Format(dateFormat)

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	signed, canonicalHeaders := canonicalHeaders(req)
	canonical := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signed,
		payloadHash,
	}, "\n")

	scope := credentialScope(date, region, service)
	toSign := strings.Join([]string{algorithm, stamp, scope, hashHex([]byte(canonical))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(cr.SecretKey, date, region, service), []byte(toSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, cr.AccessKey, scope, signed, signature))
}

// presign returns a URL carrying the signature in its query string, valid for
// ttl. Readers fetch these directly from storage, so bytes never pass through
// this service.
func presign(method string, u *url.URL, cr credentials, region, service string, ttl time.Duration, now time.Time) string {
	stamp := now.UTC().Format(stampFormat)
	date := now.UTC().Format(dateFormat)
	scope := credentialScope(date, region, service)

	q := u.Query()
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", cr.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", stamp)
	q.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")

	canonical := strings.Join([]string{
		method,
		canonicalURI(u),
		canonicalQuery(q),
		"host:" + u.Host + "\n",
		"host",
		unsignedPayload,
	}, "\n")

	toSign := strings.Join([]string{algorithm, stamp, scope, hashHex([]byte(canonical))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(cr.SecretKey, date, region, service), []byte(toSign)))

	// The signature is appended rather than sorted in: it is not part of what
	// was signed, and every other implementation puts it last.
	signed := *u
	signed.RawQuery = canonicalQuery(q) + "&X-Amz-Signature=" + signature
	return signed.String()
}

func credentialScope(date, region, service string) string {
	return strings.Join([]string{date, region, service, terminator}, "/")
}

// canonicalHeaders signs host, content-type, and every x-amz-* header. Those
// are the ones that change what the request means; signing more would only
// break on a proxy that rewrites them.
func canonicalHeaders(req *http.Request) (signed, canonical string) {
	values := map[string]string{"host": req.Host}
	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			values[lower] = strings.TrimSpace(strings.Join(vs, ","))
		}
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(values[name])
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalURI re-encodes the decoded path rather than reusing
// url.EscapedPath: Go and AWS disagree on a few bytes (notably '+'), and the
// signature must match byte for byte.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return uriEncode(u.Path, false)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(q))
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is RFC 3986 percent-encoding with AWS's unreserved set. Go's own
// escapers each differ from it somewhere, so this is spelled out.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte('/')
			}
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte(terminator))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
