package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/video"
)

// login proves who you are to a service and keeps the token it gives back.
//
// The device flow is used rather than a password or a browser redirect because
// the caller here is a terminal, or an agent in one: it can print a code and
// wait, and it has nowhere to receive a redirect.
func loginCommand(args []string) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	url := flags.String("url", "", "the service to sign in to, e.g. https://assets.example.com")
	if err := flags.Parse(args); err != nil {
		return err
	}

	target := strings.TrimRight(*url, "/")
	if target == "" {
		if existing, err := loadCredentials(); err == nil {
			target = existing.URL
		}
	}
	if target == "" {
		return errors.New("login: --url is required the first time")
	}

	c := newClient(credentials{URL: target})

	var device struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if _, err := c.postJSON("/v1/auth/github/start", struct{}{}, &device); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	fmt.Printf("\n  Open %s\n  and enter:  %s\n\n", device.VerificationURI, device.UserCode)

	interval := time.Duration(max(device.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(device.ExpiresIn, 300)) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		var issued struct {
			Status    string    `json:"status"`
			Token     string    `json:"token"`
			Handle    string    `json:"handle"`
			Namespace string    `json:"namespace"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		status, err := c.postJSON("/v1/auth/github/token",
			map[string]string{"device_code": device.DeviceCode}, &issued)
		if err != nil {
			return fmt.Errorf("login: %w", err)
		}
		if status == http.StatusAccepted {
			// GitHub asks for a slower cadence by saying so.
			if issued.Status == "slow_down" {
				interval += 5 * time.Second
			}
			continue
		}

		path, err := saveCredentials(credentials{
			URL: target, Token: issued.Token, Handle: issued.Handle,
			Namespace: issued.Namespace, ExpiresAt: issued.ExpiresAt,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Signed in as %s. Your namespace is %s.\nToken saved in %s\n",
			issued.Handle, issued.Namespace, path)
		return nil
	}
	return errors.New("login: that code expired before it was approved")
}

// uploadCommand stores a file and prints where it ended up.
func uploadCommand(args []string) error {
	flags := flag.NewFlagSet("upload", flag.ContinueOnError)
	namespace := flags.String("namespace", "", "namespace to upload into (defaults to your own)")
	name := flags.String("name", "", "filename to record (defaults to the file's own)")
	private := flags.Bool("private", false, "store it privately, reachable only through a signed URL")
	quiet := flags.Bool("quiet", false, "print only the URL")
	derived := flags.Bool("derive", false, "make the smaller copies here and upload them too")
	preset := flags.String("preset", video.DefaultPreset, "with --derive: libx264 speed against size")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("upload: expected one file, e.g. asset-service upload diagram.png")
	}

	saved, err := loadCredentials()
	if err != nil {
		return err
	}
	if *namespace == "" {
		*namespace = saved.Namespace
	}
	if *namespace == "" {
		return errors.New("upload: --namespace is required")
	}

	path := flags.Arg(0)
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	filename := *name
	if filename == "" {
		filename = filepath.Base(path)
	}

	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	query := "?namespace=" + urlValue(*namespace) + "&filename=" + urlValue(filename)
	if *private {
		query += "&visibility=private"
	}

	var manifest struct {
		Key              string `json:"key"`
		URL              string `json:"url"`
		Size             int64  `json:"size"`
		RenditionsStatus string `json:"renditions_status"`
	}
	c := newClient(saved)
	status, err := c.do(http.MethodPost, "/v1/assets"+query,
		bytes.NewReader(body), contentType, &manifest)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	// --derive spends this machine's cores instead of the service's. It is the
	// right way round for a big video: the encode happens where the file
	// already is, and the service never queues work for anybody else to do.
	if *derived && manifest.RenditionsStatus == "pending" {
		options := derive.Options{Video: video.Options{Preset: *preset}}
		count, err := build(context.Background(), c, job{
			Key: manifest.Key, ContentType: contentType, LocalPath: path,
		}, options)
		if err != nil {
			// The original is stored; the copies are not. Leave the job queued
			// so a worker can try, and say so rather than pretending.
			fmt.Fprintf(os.Stderr, "upload: made the original but not its copies: %v\n", err)
		} else {
			if err := finish(c, manifest.Key, "", false); err != nil {
				return err
			}
			manifest.RenditionsStatus = "ready"
			if !*quiet {
				fmt.Printf("made %d smaller copies here\n", count)
			}
		}
	}

	if *quiet {
		fmt.Println(manifest.URL)
		return nil
	}
	verb := "stored"
	if status == http.StatusOK {
		verb = "already stored"
	}
	fmt.Printf("%s %s (%d bytes)\n%s\n", verb, manifest.Key, manifest.Size, manifest.URL)
	if manifest.RenditionsStatus == "pending" {
		fmt.Println("smaller copies are queued; --derive would have made them here instead")
	}
	return nil
}

// remoteKeys is `keys` against a running service rather than a database.
func remoteKeys(args []string) error {
	saved, err := loadCredentials()
	if err != nil {
		return err
	}
	c := newClient(saved)

	switch args[0] {
	case "add":
		if len(args) < 3 {
			return errors.New("keys add: expected a name and at least one scope")
		}
		var issued struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if _, err := c.postJSON("/v1/keys", map[string]any{
			"name": args[1], "scopes": args[2:],
		}, &issued); err != nil {
			return fmt.Errorf("keys add: %w", err)
		}
		fmt.Println(issued.Token)
		return nil

	case "list":
		var listing struct {
			Keys []struct {
				Name      string    `json:"name"`
				Scopes    []string  `json:"scopes"`
				Revoked   bool      `json:"revoked"`
				ExpiresAt time.Time `json:"expires_at"`
			} `json:"keys"`
		}
		if _, err := c.do(http.MethodGet, "/v1/keys", nil, "", &listing); err != nil {
			return fmt.Errorf("keys list: %w", err)
		}
		for _, key := range listing.Keys {
			state := "active"
			if key.Revoked {
				state = "revoked"
			}
			expires := "never"
			if !key.ExpiresAt.IsZero() {
				expires = key.ExpiresAt.Format(time.DateOnly)
			}
			fmt.Printf("%-24s %-8s %-12s %s\n", key.Name, state, expires, strings.Join(key.Scopes, " "))
		}
		return nil

	case "revoke":
		if len(args) != 2 {
			return errors.New("keys revoke: expected one name")
		}
		if _, err := c.postJSON("/v1/keys/"+urlValue(args[1])+"/revoke", struct{}{}, nil); err != nil {
			return fmt.Errorf("keys revoke: %w", err)
		}
		fmt.Printf("revoked %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("keys: unknown subcommand %q", args[0])
	}
}

// urlValue escapes one query parameter or path segment.
func urlValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
