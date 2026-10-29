package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreateGeneratesThenReusesCertificate(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate(dir, []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" || len(leaf.IPAddresses) != 1 {
		t.Fatalf("unexpected SANs: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}

	second, err := LoadOrCreate(dir, []string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Certificate[0]) != string(first.Certificate[0]) {
		t.Fatal("existing certificate must be reused when the names match, whatever their order")
	}
}

func TestLoadOrCreateRegeneratesWhenNamesChange(t *testing.T) {
	dir := t.TempDir()
	first, _ := LoadOrCreate(dir, []string{"localhost"})

	second, err := LoadOrCreate(dir, []string{"localhost", "100.64.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Certificate[0]) == string(first.Certificate[0]) {
		t.Fatal("a changed -san must produce a new certificate")
	}
	leaf, _ := x509.ParseCertificate(second.Certificate[0])
	if len(leaf.IPAddresses) != 1 {
		t.Fatalf("new certificate lacks the IP: %v", leaf.IPAddresses)
	}
}

func TestLoadOrCreateRegeneratesNearExpiry(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost"}
	if err := generate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), hosts, 10*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	old, _ := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))

	renewed, err := LoadOrCreate(dir, hosts)
	if err != nil {
		t.Fatal(err)
	}
	if string(renewed.Certificate[0]) == string(old.Certificate[0]) {
		t.Fatal("a certificate inside the renewal window must be replaced")
	}
	leaf, _ := x509.ParseCertificate(renewed.Certificate[0])
	if time.Until(leaf.NotAfter) < 365*24*time.Hour {
		t.Fatalf("renewed certificate expires too soon: %v", leaf.NotAfter)
	}
}

func TestLoadOrCreateRecoversFromDamagedFiles(t *testing.T) {
	for name, damage := range map[string]func(cert, key string){
		"truncated cert": func(cert, _ string) { _ = os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\nMII"), 0o644) },
		"empty key":      func(_, key string) { _ = os.WriteFile(key, nil, 0o600) },
		"missing key":    func(_, key string) { _ = os.Remove(key) },
	} {
		dir := t.TempDir()
		cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
		if _, err := LoadOrCreate(dir, []string{"localhost"}); err != nil {
			t.Fatal(err)
		}
		damage(cert, key)

		if _, err := LoadOrCreate(dir, []string{"localhost"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
