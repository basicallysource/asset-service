package api

// An upload in parts, for a client whose requests reach this service through
// an edge that caps a request body below the size of its file: a CDN's proxy
// commonly stops at 100 MB, well short of a video. The client opens an upload
// saying what it is and how long, sends the bytes in order over as many
// requests as it needs, each saying where its bytes start, and finishes it.
// Finishing stores the assembled bytes exactly as one POST /v1/assets would:
// the same checks, the same key, the same answer.
//
//	POST  /v1/uploads?namespace=&visibility=&filename=  Content-Type, Upload-Length
//	PATCH /v1/uploads/{id}                               Upload-Offset: where this body starts
//	POST  /v1/uploads/{id}/finish
//
// An open upload is held in memory, its bytes in a file in the spool folder,
// until it is finished or an hour passes without a part. A restart forgets the
// open ones, and their clients start again. Only whoever opened an upload may
// add to it or finish it; to anyone else it does not exist.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/httpx"
)

const (
	// uploadIdle is how long an open upload waits for its next part.
	uploadIdle = time.Hour
	// uploadsOpenEach is how many uploads one caller may have open at once,
	// which keeps the spool folder under that many of its largest files.
	uploadsOpenEach = 4
	// partsPattern names the files that hold open uploads, apart from the
	// bodies the asset service spools while it hashes them.
	partsPattern = "parts-*"
)

type openUpload struct {
	by      string // the principal's name and account: who may add to it
	req     assets.PutRequest
	length  int64
	file    *os.File
	offset  int64
	touched time.Time
	busy    bool // a part or the finish is being handled
}

// uploads is the open uploads. The zero value is ready.
type uploads struct {
	mu   sync.Mutex
	open map[string]*openUpload
}

func caller(p *auth.Principal) string { return p.Name + "\x00" + p.Account }

// take marks an open upload busy for one request, or says why it cannot be
// had: it is not there for this caller (false), or a request is already on it.
func (u *uploads) take(id string, p *auth.Principal) (*openUpload, bool, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	up, ok := u.open[id]
	if !ok || up.by != caller(p) {
		return nil, false, false
	}
	if up.busy {
		return up, true, false
	}
	up.busy = true
	return up, true, true
}

func (u *uploads) release(up *openUpload) {
	u.mu.Lock()
	defer u.mu.Unlock()
	up.busy = false
	up.touched = time.Now()
}

func (u *uploads) drop(id string, up *openUpload) {
	u.mu.Lock()
	delete(u.open, id)
	u.mu.Unlock()
	up.file.Close()
	os.Remove(up.file.Name())
}

// sweep forgets the uploads nobody has added to for uploadIdle.
func (u *uploads) sweep(now time.Time) {
	u.mu.Lock()
	var stale []*openUpload
	for id, up := range u.open {
		if !up.busy && now.Sub(up.touched) > uploadIdle {
			delete(u.open, id)
			stale = append(stale, up)
		}
	}
	u.mu.Unlock()
	for _, up := range stale {
		up.file.Close()
		os.Remove(up.file.Name())
	}
}

// RemoveAbandonedUploads deletes the files of uploads a previous run left
// open, which nothing can finish now. It only looks in a spool folder of the
// service's own: the system temp directory is shared.
func RemoveAbandonedUploads(spoolDir string) {
	if spoolDir == "" {
		return
	}
	names, _ := filepath.Glob(filepath.Join(spoolDir, partsPattern))
	for _, name := range names {
		os.Remove(name)
	}
}

type uploadState struct {
	ID        string    `json:"id"`
	Offset    int64     `json:"offset"`
	Length    int64     `json:"length"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) openUpload(w http.ResponseWriter, r *http.Request) {
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length <= 0 {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "Upload-Length must say the whole size in bytes")
		return
	}
	req, ok := s.uploadRequest(w, r, length)
	if !ok {
		return
	}
	max := req.MaxBytes
	if max <= 0 || max > s.Assets.MaxBytes {
		max = s.Assets.MaxBytes
	}
	if length > max {
		httpx.Error(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge, "the upload is larger than this service accepts")
		return
	}

	now := time.Now()
	s.uploads.sweep(now)
	principal := auth.From(r.Context())
	s.uploads.mu.Lock()
	open := 0
	for _, up := range s.uploads.open {
		if up.by == caller(principal) {
			open++
		}
	}
	s.uploads.mu.Unlock()
	if open >= uploadsOpenEach {
		httpx.Error(w, http.StatusTooManyRequests, httpx.CodeRateLimited, "too many uploads open at once: finish one first")
		return
	}

	file, err := os.CreateTemp(s.Assets.SpoolDir, partsPattern)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		file.Close()
		os.Remove(file.Name())
		s.writeAssetError(w, r, err)
		return
	}
	id := hex.EncodeToString(raw)

	s.uploads.mu.Lock()
	if s.uploads.open == nil {
		s.uploads.open = map[string]*openUpload{}
	}
	s.uploads.open[id] = &openUpload{by: caller(principal), req: req, length: length, file: file, touched: now}
	s.uploads.mu.Unlock()

	w.Header().Set("Location", "/v1/uploads/"+id)
	httpx.JSON(w, http.StatusCreated, uploadState{ID: id, Length: length, ExpiresAt: now.Add(uploadIdle).UTC()})
}

// owned finds the caller's open upload and holds it for this request, or
// answers why not. The caller releases or drops what it gets.
func (s *Server) owned(w http.ResponseWriter, r *http.Request) (string, *openUpload, bool) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return "", nil, false
	}
	id := r.PathValue("id")
	up, found, held := s.uploads.take(id, principal)
	if !found {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "no such upload")
		return "", nil, false
	}
	if !held {
		httpx.Error(w, http.StatusConflict, httpx.CodeConflict, "another request is already writing this upload")
		return "", nil, false
	}
	return id, up, true
}

// conflict says where an upload stands, for a client whose idea of it is wrong.
func conflict(w http.ResponseWriter, up *openUpload, message string) {
	w.Header().Set("Upload-Offset", strconv.FormatInt(up.offset, 10))
	httpx.Error(w, http.StatusConflict, httpx.CodeConflict, message)
}

func (s *Server) appendUpload(w http.ResponseWriter, r *http.Request) {
	_, up, ok := s.owned(w, r)
	if !ok {
		return
	}
	defer s.uploads.release(up)

	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset != up.offset {
		conflict(w, up, "Upload-Offset must be where the upload stands")
		return
	}

	// A part either lands whole or not at all: anything short of that is
	// cut off again, so a client that lost a request sends the same part.
	if _, err := up.file.Seek(up.offset, io.SeekStart); err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	n, err := io.Copy(up.file, io.LimitReader(r.Body, up.length-up.offset))
	if err == nil {
		var extra [1]byte
		if m, _ := r.Body.Read(extra[:]); m > 0 {
			err = assets.ErrTooLarge
		}
	}
	if err != nil {
		up.file.Truncate(up.offset)
		if errors.Is(err, assets.ErrTooLarge) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge, "the part goes past the upload's length")
			return
		}
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "the part did not arrive whole: send it again")
		return
	}
	up.offset += n
	w.Header().Set("Upload-Offset", strconv.FormatInt(up.offset, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finishUpload(w http.ResponseWriter, r *http.Request) {
	id, up, ok := s.owned(w, r)
	if !ok {
		return
	}
	if up.offset != up.length {
		conflict(w, up, "the upload is not all here yet")
		s.uploads.release(up)
		return
	}
	defer s.uploads.drop(id, up)
	if _, err := up.file.Seek(0, io.SeekStart); err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	req := up.req
	req.Body = up.file
	s.put(w, r, req)
}
