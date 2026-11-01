package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestZeroSendsTheDocumentedRequest(t *testing.T) {
	var got struct {
		auth, contentType, method, path string
		body                            outbound
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth, got.contentType, got.method, got.path = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"1","status":"queued"}`))
	}))
	defer server.Close()

	if err := NewZero(server.URL+"/", "s3cret").Send(context.Background(), "amy@example.com", "Hello", "Body"); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api/outbound" || got.auth != "Bearer s3cret" || !strings.HasPrefix(got.contentType, "application/json") {
		t.Fatalf("unexpected request %+v", got)
	}
	if got.body != (outbound{To: "amy@example.com", Subject: "Hello", Text: "Body"}) {
		t.Fatalf("unexpected body %+v", got.body)
	}
}

func TestZeroMapsStatusesToErrorsWithoutLeakingSecrets(t *testing.T) {
	cases := map[int]error{
		http.StatusUnauthorized:        ErrUnauthorized,
		http.StatusUnprocessableEntity: ErrRejected,
		http.StatusTooManyRequests:     ErrRateLimited,
		http.StatusInternalServerError: ErrUnavailable,
		http.StatusBadGateway:          ErrUnavailable,
		http.StatusOK:                  ErrUnavailable,
	}
	for status, want := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("leaky body amy@example.com"))
		}))
		err := NewZero(server.URL, "s3cret").Send(context.Background(), "amy@example.com", "s", "t")
		server.Close()
		if !errors.Is(err, want) {
			t.Fatalf("status %d: got %v, want %v", status, err, want)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "amy@example.com") {
			t.Fatalf("status %d: error leaks a secret: %v", status, err)
		}
	}
}

func TestZeroNeverRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_ = NewZero(server.URL, "t").Send(context.Background(), "a@example.com", "s", "t")
	if calls.Load() != 1 {
		t.Fatalf("got %d calls, a failed send must not be repeated", calls.Load())
	}
}

func TestZeroTimesOut(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		<-release
	}))
	defer server.Close()
	defer close(release)

	zero := NewZero(server.URL, "t")
	zero.SetTimeout(50 * time.Millisecond)
	if err := zero.Send(context.Background(), "a@example.com", "s", "t"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("got %d calls after a timeout", calls.Load())
	}
}

func TestZeroDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer elsewhere.Close()
	server := httptest.NewServer(http.RedirectHandler(elsewhere.URL, http.StatusTemporaryRedirect))
	defer server.Close()

	if err := NewZero(server.URL, "t").Send(context.Background(), "a@example.com", "s", "t"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestZeroUnreachableIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	if err := NewZero(url, "t").Send(context.Background(), "a@example.com", "s", "t"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.json")

	if _, ok, err := LoadConfig(path); ok || err != nil {
		t.Fatalf("a missing file must mean off without error, got %v %v", ok, err)
	}

	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"url":"https://zero.example.com","token":"abc","portal_url":"https://files.example.com"}`)
	config, ok, err := LoadConfig(path)
	if !ok || err != nil || config.Token != "abc" {
		t.Fatalf("got %+v %v %v", config, ok, err)
	}

	for name, content := range map[string]string{
		"not json":           `nope`,
		"no token":           `{"url":"https://zero.example.com","portal_url":"https://files.example.com"}`,
		"plain http url":     `{"url":"http://zero.example.com","token":"abc","portal_url":"https://files.example.com"}`,
		"plain http portal":  `{"url":"https://zero.example.com","token":"abc","portal_url":"http://files.example.com"}`,
		"no portal url":      `{"url":"https://zero.example.com","token":"abc"}`,
		"credentials in url": `{"url":"https://u:p@zero.example.com","token":"abc","portal_url":"https://files.example.com"}`,
	} {
		write(content)
		_, ok, err := LoadConfig(path)
		if ok || err == nil {
			t.Fatalf("%s: must be refused, got ok=%v err=%v", name, ok, err)
		}
		if strings.Contains(err.Error(), "abc") {
			t.Fatalf("%s: error leaks the token", name)
		}
	}

	write(`{"url":"http://127.0.0.1:8080","token":"abc","portal_url":"http://localhost:9"}`)
	if _, ok, err := LoadConfig(path); !ok || err != nil {
		t.Fatalf("loopback http is for local development, got %v %v", ok, err)
	}
}
