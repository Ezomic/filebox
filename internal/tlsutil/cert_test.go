package tlsutil

import (
	"crypto/x509"
	"testing"
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

	second, err := LoadOrCreate(dir, []string{"other.example"})
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Certificate[0]) != string(first.Certificate[0]) {
		t.Fatal("existing certificate must be reused")
	}
}
