package config

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createTestCertificate(t *testing.T, name string, isCA bool, parent *x509.Certificate, signer crypto.Signer, notBefore, notAfter time.Time) (string, *x509.Certificate, crypto.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(notBefore.UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	if isCA {
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	} else {
		template.KeyUsage = x509.KeyUsageDigitalSignature
	}
	if parent == nil {
		parent, signer = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), certificate, key
}

func testRootCA(t *testing.T, name string) string {
	t.Helper()
	now := time.Now()
	value, _, _ := createTestCertificate(t, name, true, nil, nil, now.Add(-time.Hour), now.Add(time.Hour))
	return value
}

func TestRootCACertificateInlineAndPathSources(t *testing.T) {
	doc, template := validVMDocument(t, "")
	base := t.TempDir()
	inline := testRootCA(t, "inline")
	fileValue := testRootCA(t, "file")
	if err := os.Mkdir(filepath.Join(base, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "certs", "root.pem")
	if err := os.WriteFile(path, []byte("\n"+fileValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc = []byte(strings.Replace(string(doc), "    ssh_authorized_keys:\n", `    root_ca_certificates:
      - pem: "${ROOT_CA:?root CA required}"
      - path: certs/root.pem
    ssh_authorized_keys:
`, 1))
	cfg, _, err := Parse(doc, base, func(name string) (string, bool) {
		switch name {
		case "IMAGE":
			return template, true
		case "KEY":
			return testKey(t), true
		case "ROOT_CA":
			return "\n" + inline + "\n", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	vm, _ := cfg.LookupVM("vm")
	if len(vm.RootCACertificates) != 2 {
		t.Fatalf("root CAs = %#v", vm.RootCACertificates)
	}
	if vm.RootCACertificates[0].PEM != inline || vm.RootCACertificates[0].CanonicalPEM != inline {
		t.Fatalf("inline certificate was not canonicalized: %#v", vm.RootCACertificates[0])
	}
	if vm.RootCACertificates[1].Path != path || vm.RootCACertificates[1].CanonicalPEM != fileValue {
		t.Fatalf("path certificate was not resolved: %#v", vm.RootCACertificates[1])
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "root_ca_certificates:") || !strings.Contains(string(out), "path: "+path) || strings.Contains(string(out), "CanonicalPEM") {
		t.Fatalf("unexpected normalized config:\n%s", out)
	}
}

func TestRootCACertificateValidation(t *testing.T) {
	now := time.Now()
	rootPEM, root, rootKey := createTestCertificate(t, "root", true, nil, nil, now.Add(-time.Hour), now.Add(time.Hour))
	intermediatePEM, _, _ := createTestCertificate(t, "intermediate", true, root, rootKey, now.Add(-time.Hour), now.Add(time.Hour))
	nonCAPEM, _, _ := createTestCertificate(t, "not-ca", false, nil, nil, now.Add(-time.Hour), now.Add(time.Hour))
	expiredPEM, _, _ := createTestCertificate(t, "expired", true, nil, nil, now.Add(-2*time.Hour), now.Add(-time.Hour))
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a key")})

	tests := map[string]struct {
		values []*RootCACertificate
		want   string
	}{
		"null entry":       {[]*RootCACertificate{nil}, "is null"},
		"missing source":   {[]*RootCACertificate{{}}, "exactly one"},
		"both sources":     {[]*RootCACertificate{{PEM: rootPEM, Path: "root.pem"}}, "exactly one"},
		"not PEM":          {[]*RootCACertificate{{PEM: "garbage"}}, "not PEM encoded"},
		"wrong block":      {[]*RootCACertificate{{PEM: string(privateKey)}}, "want CERTIFICATE"},
		"leading data":     {[]*RootCACertificate{{PEM: "prefix\n" + rootPEM}}, "data before its PEM block"},
		"additional block": {[]*RootCACertificate{{PEM: rootPEM + rootPEM}}, "additional PEM blocks"},
		"not a CA":         {[]*RootCACertificate{{PEM: nonCAPEM}}, "not a root CA"},
		"not self-signed":  {[]*RootCACertificate{{PEM: intermediatePEM}}, "subject and issuer differ"},
		"duplicate":        {[]*RootCACertificate{{PEM: rootPEM}, {PEM: "\n" + rootPEM}}, "duplicates entry 0"},
		"missing path":     {[]*RootCACertificate{{Path: "missing.pem"}}, "no such file"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := normalizeRootCACertificates("vm", tt.values, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	directory := t.TempDir()
	if err := normalizeRootCACertificates("vm", []*RootCACertificate{{Path: directory}}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory error = %v", err)
	}
	duplicatePath := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(duplicatePath, []byte(rootPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := normalizeRootCACertificates("vm", []*RootCACertificate{{PEM: rootPEM}, {Path: duplicatePath}}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "duplicates entry 0") {
		t.Fatalf("cross-source duplicate error = %v", err)
	}
	if err := normalizeRootCACertificates("vm", []*RootCACertificate{{PEM: expiredPEM}}, t.TempDir()); err != nil {
		t.Fatalf("validity dates must not be checked: %v", err)
	}
}

func TestRootCACertificateRejectsInvalidSelfSignature(t *testing.T) {
	value := testRootCA(t, "corrupt")
	block, _ := pem.Decode([]byte(value))
	block.Bytes[len(block.Bytes)-1] ^= 1
	corrupt := string(pem.EncodeToMemory(block))
	err := normalizeRootCACertificates("vm", []*RootCACertificate{{PEM: corrupt}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "valid self-signature") {
		t.Fatalf("error = %v", err)
	}
}
