package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"sync"
	"time"
)

const recheckEvery = 30 * time.Second

// Reloader serves a certificate from disk and picks up a renewed one without a restart.
// A renewal that fails to load keeps the previous certificate in service.
type Reloader struct {
	certPath, keyPath string

	mu       sync.Mutex
	current  *tls.Certificate
	notAfter time.Time
	checked  time.Time
}

func NewReloader(certPath, keyPath string) (*Reloader, error) {
	r := &Reloader{certPath: certPath, keyPath: keyPath}
	if err := r.load(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.checked) > recheckEvery {
		_ = r.load()
	}

	return r.current, nil
}

// NotAfter is when the certificate in service expires.
func (r *Reloader) NotAfter() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.checked) > recheckEvery {
		_ = r.load()
	}

	return r.notAfter
}

func (r *Reloader) load() error {
	r.checked = time.Now()

	pair, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	r.current = &pair
	r.notAfter = leaf.NotAfter

	return nil
}
