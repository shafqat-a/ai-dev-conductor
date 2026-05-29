package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/shafqat-a/ai-dev-conductor/internal/session"
	"github.com/shafqat-a/ai-dev-conductor/internal/store"
)

func testShell(t *testing.T) string {
	t.Helper()
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	t.Skip("no shell available")
	return ""
}

// newSessionInDir creates a raw-PTY session and drives its shell to cd into dir,
// so CWD() (via /proc/<pid>/cwd) reports a directory the test controls.
func newSessionInDir(t *testing.T, dir string) (*session.Manager, *session.Session) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mgr := session.NewManager(testShell(t), t.TempDir(), st, 0, 0, false)
	t.Cleanup(mgr.CloseAll)

	s, err := mgr.Create("xfer")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := s.WriteInput([]byte("cd " + want + "\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cwd, err := s.CWD(); err == nil {
			if resolved, _ := filepath.EvalSymlinks(cwd); resolved == want {
				return mgr, s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shell never reported the expected working directory")
	return nil, nil
}

// withRouteID injects a chi {id} URL param so the handlers can resolve the session.
func withRouteID(req *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func multipartUpload(t *testing.T, filename string, body []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	fw.Write(body)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestHandleUploadLandsInCWD(t *testing.T) {
	dir := t.TempDir()
	mgr, s := newSessionInDir(t, dir)

	body, contentType := multipartUpload(t, "hello.txt", []byte("payload"))
	req := withRouteID(httptest.NewRequest(http.MethodPost, "/upload", body), s.ID)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	HandleUpload(mgr, 1<<20)(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rr.Code, rr.Body)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("uploaded file not found: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("contents = %q, want %q", got, "payload")
	}
}

func TestHandleUploadStripsTraversal(t *testing.T) {
	dir := t.TempDir()
	mgr, s := newSessionInDir(t, dir)

	body, contentType := multipartUpload(t, "../../escape.txt", []byte("nope"))
	req := withRouteID(httptest.NewRequest(http.MethodPost, "/upload", body), s.ID)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	HandleUpload(mgr, 1<<20)(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rr.Code, rr.Body)
	}
	// The base name is kept; the traversal prefix is discarded, so it lands in CWD.
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatalf("expected escape.txt inside CWD: %v", err)
	}
	parent := filepath.Dir(dir)
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("traversal escaped the working directory")
	}
}

func TestHandleUploadRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	mgr, s := newSessionInDir(t, dir)

	body, contentType := multipartUpload(t, "big.bin", bytes.Repeat([]byte("x"), 4096))
	req := withRouteID(httptest.NewRequest(http.MethodPost, "/upload", body), s.ID)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	HandleUpload(mgr, 512)(rr, req) // cap below the body size

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d (%s)", rr.Code, rr.Body)
	}
}

func TestHandleDownloadServesFromCWD(t *testing.T) {
	dir := t.TempDir()
	mgr, s := newSessionInDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("grab me"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := withRouteID(httptest.NewRequest(http.MethodGet, "/download?path=data.txt", nil), s.ID)
	rr := httptest.NewRecorder()

	HandleDownload(mgr)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rr.Code, rr.Body)
	}
	if rr.Body.String() != "grab me" {
		t.Fatalf("body = %q, want %q", rr.Body.String(), "grab me")
	}
}

func TestHandleDownloadRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	mgr, s := newSessionInDir(t, dir)
	// A secret outside the CWD that must never be served.
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secret) })

	req := withRouteID(httptest.NewRequest(http.MethodGet, "/download?path=../secret.txt", nil), s.ID)
	rr := httptest.NewRecorder()

	HandleDownload(mgr)(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d (%s)", rr.Code, rr.Body)
	}
}

func TestConfinedPath(t *testing.T) {
	base := "/srv/work"
	cases := []struct {
		rel     string
		wantErr bool
	}{
		{"file.txt", false},
		{"sub/dir/file.txt", false},
		{"../escape", true},
		{"../../etc/passwd", true},
		{"/etc/passwd", false}, // absolute is joined under base, not an escape
		{"sub/../file.txt", false},
		{"sub/../../escape", true},
	}
	for _, c := range cases {
		_, err := confinedPath(base, c.rel)
		if (err != nil) != c.wantErr {
			t.Errorf("confinedPath(%q): err=%v, wantErr=%v", c.rel, err, c.wantErr)
		}
	}
}
