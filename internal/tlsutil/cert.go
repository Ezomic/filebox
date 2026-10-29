package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	validity    = 5 * 365 * 24 * time.Hour
	renewWindow = 30 * 24 * time.Hour
)

func LoadOrCreate(dir string, hosts []string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if pair, ok := loadUsable(certPath, keyPath, hosts); ok {
		return pair, nil
	}
	if err := generate(certPath, keyPath, hosts, validity); err != nil {
		return tls.Certificate{}, err
	}

	return tls.LoadX509KeyPair(certPath, keyPath)
}

// A missing, truncated, mismatched, soon-to-expire or differently-named certificate is replaced.
func loadUsable(certPath, keyPath string, hosts []string) (tls.Certificate, bool) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.Add(renewWindow).After(leaf.NotAfter) {
		return tls.Certificate{}, false
	}
	if !slices.Equal(names(leaf.DNSNames, leaf.IPAddresses), names(splitHosts(hosts))) {
		return tls.Certificate{}, false
	}

	return pair, true
}

func splitHosts(hosts []string) ([]string, []net.IP) {
	var dns []string
	var ips []net.IP
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, host)
		}
	}

	return dns, ips
}

func names(dns []string, ips []net.IP) []string {
	all := slices.Clone(dns)
	for _, ip := range ips {
		all = append(all, ip.String())
	}
	slices.Sort(all)

	return slices.Compact(all)
}

func generate(certPath, keyPath string, hosts []string, validFor time.Duration) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "filebox"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	template.DNSNames, template.IPAddresses = splitHosts(hosts)

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	return writeFile(certPath, certPEM, 0o644)
}

func writeFile(path string, content []byte, mode os.FileMode) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, content, mode); err != nil {
		return err
	}

	return os.Rename(temp, path)
}
