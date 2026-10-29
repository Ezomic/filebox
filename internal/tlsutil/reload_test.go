package tlsutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReloaderPicksUpRenewalAndKeepsOldOnBadFile(t *testing.T) {
	dir := t.TempDir()
	if err := generate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), []string{"a.example"}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	r, err := NewReloader(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	first := r.NotAfter()

	if err := generate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), []string{"a.example"}, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	r.checked = time.Time{}
	if got := r.NotAfter(); !got.After(first) {
		t.Fatalf("renewed certificate not loaded: %v vs %v", got, first)
	}

	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.checked = time.Time{}
	cert, err := r.GetCertificate(nil)
	if err != nil || cert == nil {
		t.Fatalf("a broken renewal must keep the old certificate: %v %v", cert, err)
	}
}

func TestReloaderFailsWithoutCertificate(t *testing.T) {
	if _, err := NewReloader("/no/cert.pem", "/no/key.pem"); err == nil {
		t.Fatal("missing files must fail at startup")
	}
}
