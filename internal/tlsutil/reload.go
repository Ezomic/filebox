package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"slices"
	"strings"
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
	host     string
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

// Host is the first plain DNS name of the certificate in service, the name visitors reach the portal by. A
// wildcard name says nothing about which name that is, so it is skipped.
func (r *Reloader) Host() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.checked) > recheckEvery {
		_ = r.load()
	}

	return r.host
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
	r.host = ""
	if i := slices.IndexFunc(leaf.DNSNames, func(name string) bool { return !strings.HasPrefix(name, "*") }); i >= 0 {
		r.host = leaf.DNSNames[i]
	}

	return nil
}
