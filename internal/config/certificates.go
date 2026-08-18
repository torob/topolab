package config

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func normalizeRootCACertificates(vm string, values []*RootCACertificate, baseDir string) error {
	seen := map[[sha256.Size]byte]int{}
	for i, value := range values {
		where := fmt.Sprintf("VM %q root_ca_certificates entry %d", vm, i)
		if value == nil {
			return fmt.Errorf("%s is null", where)
		}
		if (value.PEM == "") == (value.Path == "") {
			return fmt.Errorf("%s must set exactly one of pem or path", where)
		}

		contents := []byte(value.PEM)
		if value.Path != "" {
			path := value.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			absolute, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("%s: resolve path: %w", where, err)
			}
			value.Path = filepath.Clean(absolute)
			info, err := os.Stat(value.Path)
			if err != nil {
				return fmt.Errorf("%s path %q: %w", where, value.Path, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s path %q is not a regular file", where, value.Path)
			}
			contents, err = os.ReadFile(value.Path)
			if err != nil {
				return fmt.Errorf("%s path %q: %w", where, value.Path, err)
			}
		}

		canonical, fingerprint, err := validateRootCACertificate(contents)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if prior, ok := seen[fingerprint]; ok {
			return fmt.Errorf("%s duplicates entry %d", where, prior)
		}
		seen[fingerprint] = i
		value.CanonicalPEM = canonical
		if value.PEM != "" {
			value.PEM = canonical
		}
	}
	return nil
}

func validateRootCACertificate(contents []byte) (string, [sha256.Size]byte, error) {
	contents = bytes.TrimSpace(contents)
	block, rest := pem.Decode(contents)
	if block == nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("certificate is not PEM encoded")
	}
	if !bytes.HasPrefix(contents, []byte("-----BEGIN "+block.Type+"-----")) {
		return "", [sha256.Size]byte{}, fmt.Errorf("certificate contains data before its PEM block")
	}
	if block.Type != "CERTIFICATE" {
		return "", [sha256.Size]byte{}, fmt.Errorf("PEM block type is %q, want CERTIFICATE", block.Type)
	}
	if strings.TrimSpace(string(rest)) != "" {
		return "", [sha256.Size]byte{}, fmt.Errorf("certificate contains additional PEM blocks or data")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("parse X.509 certificate: %w", err)
	}
	if !certificate.BasicConstraintsValid || !certificate.IsCA {
		return "", [sha256.Size]byte{}, fmt.Errorf("certificate is not a root CA")
	}
	if !bytes.Equal(certificate.RawSubject, certificate.RawIssuer) {
		return "", [sha256.Size]byte{}, fmt.Errorf("certificate subject and issuer differ")
	}
	if err := certificate.CheckSignatureFrom(certificate); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("certificate does not have a valid self-signature: %w", err)
	}
	canonical := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	return string(canonical), sha256.Sum256(certificate.Raw), nil
}
