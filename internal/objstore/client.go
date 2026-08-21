package objstore

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// service is the SigV4 service name. S3-compatible storage always signs as s3,
// whoever operates it.
const service = "s3"

// emptyPayload is the SHA-256 of no bytes, which is what a HEAD signs.
const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// digestMeta travels with every object so the bucket describes itself. The
// catalog can be rebuilt from a bucket listing; a bucket cannot be rebuilt
// from the catalog.
const digestMeta = "X-Amz-Meta-Digest"

// Every object this service stores is named after a hash of its own bytes, so
// no key's content can ever change and a year is a safe thing to promise.
const (
	publicCacheControl  = "public, max-age=31536000, immutable"
	privateCacheControl = "private, max-age=31536000, immutable"
)

// Config describes one bucket on one S3-compatible provider.
type Config struct {
	// Endpoint is the provider's regional URL, e.g.
	// https://nyc3.digitaloceanspaces.com -- bucket excluded.
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// PathStyle addresses the bucket as a path segment instead of a
	// subdomain. Some providers (and most local test servers) need it.
	PathStyle bool
	// PublicBaseURL is where readers fetch public objects: a CDN or another
	// edge in front of the same bucket. No trailing slash.
	PublicBaseURL string
	HTTPClient    *http.Client
}

// Client is an S3-compatible object store client covering exactly the three
// operations this service performs.
type Client struct {
	cfg    Config
	creds  credentials
	base   *url.URL
	http   *http.Client
	public string
	now    func() time.Time
}

var _ Store = (*Client)(nil)

// New validates cfg and returns a client for one bucket.
func New(cfg Config) (*Client, error) {
	var missing []string
	for name, v := range map[string]string{
		"endpoint": cfg.Endpoint, "region": cfg.Region, "bucket": cfg.Bucket,
		"access key": cfg.AccessKey, "secret key": cfg.SecretKey,
		"public base URL": cfg.PublicBaseURL,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("objstore: missing %s", strings.Join(missing, ", "))
	}

	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("objstore: bad endpoint %q", cfg.Endpoint)
	}

	base := &url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host}
	if cfg.PathStyle {
		base.Path = "/" + cfg.Bucket
	} else {
		base.Host = cfg.Bucket + "." + endpoint.Host
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	return &Client{
		cfg:    cfg,
		creds:  credentials{AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey},
		base:   base,
		http:   client,
		public: strings.TrimRight(cfg.PublicBaseURL, "/"),
		now:    time.Now,
	}, nil
}

func (c *Client) objectURL(key string) *url.URL {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(key, "/")
	return &u
}

// Head reports what storage holds at key. A missing object is not an error:
// it comes back as Object{Exists: false}.
func (c *Client) Head(ctx context.Context, key string) (Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.objectURL(key).String(), nil)
	if err != nil {
		return Object{}, err
	}
	signRequest(req, c.creds, c.cfg.Region, service, emptyPayload, c.now())

	resp, err := c.http.Do(req)
	if err != nil {
		return Object{}, fmt.Errorf("objstore: head %s: %w", key, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Object{}, nil
	case resp.StatusCode != http.StatusOK:
		return Object{}, &Error{Op: "head", Key: key, StatusCode: resp.StatusCode}
	}

	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return Object{
		Exists:      true,
		Size:        size,
		ContentType: resp.Header.Get("Content-Type"),
		Digest:      resp.Header.Get(digestMeta),
	}, nil
}

// Put stores body at req.Key. body must yield exactly req.Size bytes hashing
// to req.Digest; the digest is both the object's recorded metadata and the
// payload hash the signature commits to, so a corrupted upload cannot be
// signed into place under a name that says otherwise.
func (c *Client) Put(ctx context.Context, req PutRequest, body io.Reader) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(req.Key).String(), body)
	if err != nil {
		return err
	}
	r.ContentLength = req.Size
	r.Header.Set("Content-Type", req.ContentType)
	r.Header.Set(digestMeta, req.Digest)
	if req.Public {
		r.Header.Set("X-Amz-Acl", "public-read")
		r.Header.Set("Cache-Control", publicCacheControl)
	} else {
		r.Header.Set("X-Amz-Acl", "private")
		r.Header.Set("Cache-Control", privateCacheControl)
	}
	signRequest(r, c.creds, c.cfg.Region, service, req.Digest, c.now())

	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("objstore: put %s: %w", req.Key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return parseError("put", req.Key, resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// PublicURL is where anyone can fetch a public object.
func (c *Client) PublicURL(key string) string {
	return c.public + "/" + strings.TrimLeft(key, "/")
}

// SignedURL is a time-limited URL for a private object.
//
// It is signed against the storage endpoint rather than PublicBaseURL: a
// signature covers the host it was made for, and a CDN in front of the bucket
// is a different host. Private assets therefore read from origin, which is the
// right trade while they are the rare case.
func (c *Client) SignedURL(key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", errors.New("objstore: signed URL ttl must be positive")
	}
	return presign(http.MethodGet, c.objectURL(key), c.creds, c.cfg.Region, service, ttl, c.now()), nil
}

// Error is a failed storage request. Providers answer with an XML document
// whose Code is the only part worth acting on.
type Error struct {
	Op         string
	Key        string
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("objstore: %s %s: http %d", e.Op, e.Key, e.StatusCode)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func parseError(op, key string, resp *http.Response) error {
	e := &Error{Op: op, Key: key, StatusCode: resp.StatusCode}
	var doc struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if xml.Unmarshal(body, &doc) == nil {
		e.Code, e.Message = doc.Code, doc.Message
	}
	return e
}
