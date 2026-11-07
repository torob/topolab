package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

// Remote selects remote operation. It is controller input only: it is not part
// of any hash and the agent ignores it.
type Remote struct {
	SSH string `yaml:"ssh"`
}

// ValidateDestination checks an OpenSSH destination: [user@]hostname,
// ssh://[user@]hostname[:port], or a Host alias. It must not look like an ssh
// option and must not contain whitespace or control characters.
func ValidateDestination(value string) error {
	if value == "" {
		return errors.New("destination may not be empty")
	}
	if strings.HasPrefix(value, "-") {
		return errors.New("destination may not begin with '-'")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("destination may not contain whitespace or control characters")
		}
	}
	return nil
}

// Digest returns the SHA-256 of the normalized configuration. The controller
// and the agent compute it from their own parse and compare the values, which
// turns any parse divergence into a hard stop.
func Digest(cfg *Config) (string, error) {
	data, err := Marshal(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
