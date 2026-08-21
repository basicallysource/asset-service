package assets

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// HashChars is how much of the digest appears in a key. Twelve hex characters
// is 48 bits, and a collision would additionally have to land on the same
// namespace, name and extension to matter -- and even then it is caught and
// refused rather than silently overwriting, so this is a readability choice,
// not a safety one.
const HashChars = 12

// A namespace is the unit access is granted over: lowercase, dashed, short.
var namespacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// An extension is kept only when it is boring. Anything else is dropped rather
// than smuggled into a key.
var extPattern = regexp.MustCompile(`^\.[a-z0-9]{1,16}$`)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// ValidNamespace reports whether ns can name a namespace.
func ValidNamespace(ns string) bool { return namespacePattern.MatchString(ns) }

// ErrBadRequest is anything the caller can fix by asking differently.
var ErrBadRequest = errors.New("bad request")

// BuildKey composes the storage key for a set of bytes.
//
// The shape is <namespace>/<name>-<hash>.<ext>: readable enough to recognise
// in a log or a bucket listing, and self-verifying, because the hash in the
// name is the hash of the bytes stored under it. The service computes it --
// callers propose a filename, never a key -- which is what makes it impossible
// for a name to disagree with its content.
func BuildKey(namespace, filename, digest string) (string, error) {
	if !ValidNamespace(namespace) {
		return "", fmt.Errorf("%w: namespace %q must be lowercase letters, digits and dashes", ErrBadRequest, namespace)
	}
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("%w: digest must be 64 hex characters", ErrBadRequest)
	}

	base := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	ext := strings.ToLower(path.Ext(base))
	if !extPattern.MatchString(ext) {
		ext = ""
	}
	name := slug(strings.TrimSuffix(base, path.Ext(base)))
	if name == "" {
		return "", fmt.Errorf("%w: filename %q has no usable name", ErrBadRequest, filename)
	}

	return namespace + "/" + name + "-" + digest[:HashChars] + ext, nil
}

// Namespace returns the namespace a key belongs to.
func Namespace(key string) string {
	ns, _, ok := strings.Cut(key, "/")
	if !ok {
		return ""
	}
	return ns
}

// slug reduces a filename stem to the characters that are safe in a URL, a
// shell, and a bucket listing.
func slug(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	return s
}
