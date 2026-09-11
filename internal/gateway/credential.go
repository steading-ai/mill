package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// The upstream credential is read from a file and never from the
// environment: a variable is inherited by every child, and the one process
// that may hold this value is the gateway's.
const (
	// CredentialsDirectoryEnv is systemd's variable naming the directory
	// LoadCredential= delivers credentials into.
	CredentialsDirectoryEnv = "CREDENTIALS_DIRECTORY"
	// CredentialName is the file under it holding the subscription
	// credential: the output of `claude setup-token`, delivered to the
	// worker by LoadCredential from vault_claude_code_oauth_token
	// (docs/sandbox.md §Credentials).
	CredentialName = "claude-oauth"
)

// DefaultCredentialPath is where the credential is read from when nothing
// names a file: $CREDENTIALS_DIRECTORY/claude-oauth. Empty when systemd
// delivered nothing, which is a laptop rather than a deployment.
func DefaultCredentialPath() string {
	dir := os.Getenv(CredentialsDirectoryEnv)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, CredentialName)
}

// Credential is the subscription credential the gateway presents upstream.
// It renders as its fingerprint everywhere — %v, %s, a log field — so that
// the value itself reaches the upstream's Authorization header and nothing
// else.
type Credential struct {
	value string
}

// ErrNoCredential is a credential file that is missing or empty.
var ErrNoCredential = errors.New("gateway: no upstream credential")

// LoadCredential reads the credential from path. Surrounding whitespace is
// dropped, because a file written by hand ends in a newline.
func LoadCredential(path string) (Credential, error) {
	if path == "" {
		return Credential{}, fmt.Errorf("%w: no file is named; set %s or --model-credential-file", ErrNoCredential, CredentialsDirectoryEnv)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's configuration
	if err != nil {
		return Credential{}, fmt.Errorf("%w: %s: %w", ErrNoCredential, path, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return Credential{}, fmt.Errorf("%w: %s is empty", ErrNoCredential, path)
	}
	return Credential{value: value}, nil
}

// CredentialOf holds a value already in hand. The specs and the live
// sidecar seam use it; a deployment loads a file.
func CredentialOf(value string) Credential { return Credential{value: value} }

// Empty reports whether there is no credential at all.
func (c Credential) Empty() bool { return c.value == "" }

// Fingerprint is the first eight hex characters of the value's SHA-256:
// enough to tell two credentials apart in a log, and nothing a request can
// be made with.
func (c Credential) Fingerprint() string {
	if c.value == "" {
		return "none"
	}
	return fingerprint(c.value)
}

// String is the fingerprint, so a credential formatted by mistake still
// does not print.
func (c Credential) String() string { return "credential:" + c.Fingerprint() }

// GoString is String, so %#v does not print the field either.
func (c Credential) GoString() string { return c.String() }

// LogValue is String, for slog.
func (c Credential) LogValue() slog.Value { return slog.StringValue(c.String()) }

// fingerprint is how any secret is named in the gateway's log.
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}
