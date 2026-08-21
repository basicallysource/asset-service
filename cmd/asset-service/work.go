package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/video"
)

// workCommand does the service's expensive half somewhere else.
//
// Deriving wants cores; the machine a small service runs on usually has two
// and something else already using them. This claims jobs over the API, does
// the work here, and hands the bytes back -- so the box that serves assets
// never has to be the box that transcodes them, and the one that does can be
// anything with a CPU and this binary.
//
// It needs a credential with admin, and ffmpeg on PATH for video. Stopping it
// is safe at any point: a claim that goes quiet is offered to somebody else.
func workCommand(args []string) error {
	flags := flag.NewFlagSet("work", flag.ContinueOnError)
	once := flags.Bool("once", false, "drain the queue and stop, rather than waiting for more")
	poll := flags.Duration("poll", 15*time.Second, "how long to wait when there is nothing to do")
	preset := flags.String("preset", video.DefaultPreset, "libx264 speed against size")
	if err := flags.Parse(args); err != nil {
		return err
	}

	saved, err := loadCredentials()
	if err != nil {
		return err
	}
	c := newClient(saved)

	if !video.Available() {
		fmt.Fprintln(os.Stderr, "work: ffmpeg is not installed; video jobs will be reported as failed")
	}
	fmt.Printf("working for %s\n", saved.URL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	options := derive.Options{Video: video.Options{Preset: *preset}}
	for ctx.Err() == nil {
		worked, err := workOne(ctx, c, options)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		if *once {
			fmt.Println("queue is empty")
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(*poll):
		}
	}
	return nil
}

// job is one unit of work, and everything needed to do it without holding
// storage credentials.
type job struct {
	Key         string `json:"key"`
	Namespace   string `json:"namespace"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Attempts    int    `json:"attempts"`
	SourceURL   string `json:"source_url"`
}

// workOne claims a job, does it, and reports. It reports whether it found work.
func workOne(ctx context.Context, c *client, options derive.Options) (bool, error) {
	var claimed job
	status, err := c.do(http.MethodPost, "/v1/jobs/claim", nil, "", &claimed)
	if err != nil {
		return false, err
	}
	if status == http.StatusNoContent {
		return false, nil
	}

	started := time.Now()
	fmt.Printf("%s  %s\n", claimed.Key, claimed.ContentType)

	count, err := build(ctx, c, claimed, options)
	if err != nil {
		// Bytes that will never derive are not worth offering to anyone else.
		permanent := errors.Is(err, derive.ErrUnsupported)
		fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
		return true, finish(c, claimed.Key, err.Error(), permanent)
	}

	fmt.Printf("  %d renditions in %s\n", count, time.Since(started).Round(time.Second))
	return true, finish(c, claimed.Key, "", false)
}

// build derives every form of one asset and sends each back as it is made.
func build(ctx context.Context, c *client, claimed job, options derive.Options) (int, error) {
	source, err := download(ctx, claimed.SourceURL)
	if err != nil {
		return 0, err
	}
	defer os.Remove(source)

	ladder, err := derive.Ladder(ctx, source, claimed.ContentType, options)
	if err != nil {
		return 0, err
	}

	for _, rendition := range ladder {
		query := url.Values{
			"key":    {claimed.Key},
			"name":   {rendition.Name},
			"width":  {strconv.Itoa(rendition.Width)},
			"height": {strconv.Itoa(rendition.Height)},
			"ext":    {rendition.Extension},
		}
		if _, err := c.do(http.MethodPost, "/v1/jobs/renditions?"+query.Encode(),
			bytes.NewReader(rendition.Bytes), rendition.ContentType, nil); err != nil {
			return 0, fmt.Errorf("storing %s: %w", rendition.Name, err)
		}
	}
	return len(ladder), nil
}

// download fetches the original to a temporary file, straight from storage:
// the service hands out a URL rather than the bytes, so the work costs it
// nothing but the redirect.
func download(ctx context.Context, from string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch the original: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return "", fmt.Errorf("fetch the original: %s", response.Status)
	}

	file, err := os.CreateTemp("", "original-")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func finish(c *client, key, failure string, permanent bool) error {
	report, err := json.Marshal(map[string]any{"error": failure, "permanent": permanent})
	if err != nil {
		return err
	}
	_, err = c.do(http.MethodPost, "/v1/jobs/finish?key="+url.QueryEscape(key),
		bytes.NewReader(report), "application/json", nil)
	return err
}
