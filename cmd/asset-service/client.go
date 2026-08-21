package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The CLI talks to a running service the same way anything else does: over the
// API, with a token. The one exception is a host operator, who has the
// database itself and needs no service running to bootstrap one.

// credentialsPath is where a signed-in token is kept: readable only by its
// owner, outside any working tree.
func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "asset-service", "credentials.json"), nil
}

type credentials struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	Handle    string    `json:"handle,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// loadCredentials prefers the environment, so a build or a container can be
// handed a token without a file.
func loadCredentials() (credentials, error) {
	saved := credentials{
		URL:   strings.TrimRight(os.Getenv("ASSET_SERVICE_URL"), "/"),
		Token: os.Getenv("ASSET_SERVICE_TOKEN"),
	}
	if saved.URL != "" && saved.Token != "" {
		return saved, nil
	}

	path, err := credentialsPath()
	if err != nil {
		return credentials{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return credentials{}, errors.New("not signed in: run `asset-service login --url https://…`")
		}
		return credentials{}, err
	}

	var stored credentials
	if err := json.Unmarshal(body, &stored); err != nil {
		return credentials{}, fmt.Errorf("read %s: %w", path, err)
	}
	if saved.URL != "" {
		stored.URL = saved.URL
	}
	if saved.Token != "" {
		stored.Token = saved.Token
	}
	if stored.URL == "" || stored.Token == "" {
		return credentials{}, errors.New("not signed in: run `asset-service login --url https://…`")
	}
	return stored, nil
}

func saveCredentials(c credentials) (string, error) {
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(body, '\n'), 0o600)
}

// client is a caller of the service's own API.
type client struct {
	url   string
	token string
	http  *http.Client
}

func newClient(c credentials) *client {
	return &client{url: strings.TrimRight(c.URL, "/"), token: c.Token, http: &http.Client{Timeout: 5 * time.Minute}}
}

// do sends a request and decodes the answer, turning the service's own error
// envelope into a Go error so every command reports failures the same way.
func (c *client) do(method, path string, body io.Reader, contentType string, into any) (int, error) {
	req, err := http.NewRequest(method, c.url+path, body)
	if err != nil {
		return 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, c.url+path, err)
	}
	defer resp.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}

	if resp.StatusCode >= 400 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(answer, &envelope) == nil && envelope.Error.Message != "" {
			return resp.StatusCode, fmt.Errorf("%s (%s)", envelope.Error.Message, envelope.Error.Code)
		}
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(answer)))
	}

	if into != nil && len(answer) > 0 {
		if err := json.Unmarshal(answer, into); err != nil {
			return resp.StatusCode, fmt.Errorf("read the answer to %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *client) postJSON(path string, request, into any) (int, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return 0, err
	}
	return c.do(http.MethodPost, path, bytes.NewReader(body), "application/json", into)
}
