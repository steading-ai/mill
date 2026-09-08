// Package signing is the mill's commit signature.
//
// Every commit the workspace makes is signed with an SSH key owned by the
// mill's account and never handed to a session (docs/architecture.md
// §"Identity and credentials"). Verification is `git verify-commit` against an
// allowed_signers file, so a commit's provenance is checkable by anyone with
// the repository and the file.
//
// # allowed_signers
//
// OpenSSH's allowed_signers format is one signer per line:
//
//	<principals> [options] <keytype> <base64 key> [comment]
//
// [Key.AllowedSigners] renders exactly one line, whose principal is the key's
// identity — the address the mill commits under — and whose only option is
// `namespaces="git"`, so the key is trusted for git signatures and for
// nothing else. Git finds the principal by matching the signature's key
// against this file, which is why the line must carry the key the commit was
// signed with and why a rotated key means a rewritten file.
//
// The file lives beside the key, and [Key.GitConfig] points
// `gpg.ssh.allowedSignersFile` at it, so a workspace verifies against the
// same file it signs with. A project that wants to verify mill's commits
// itself copies that line into its own allowed_signers, which is the
// deployment step docs/architecture.md describes: "an SSH signing key owned
// by the mill's account in MVP, listed in each project's allowed_signers".
// Per-run SSH certificates from a CA replace this once a secrets service
// exists; the certificate's principal then names the node, and only
// [Generate] changes.
package signing

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ivy/mill/internal/workspace"
)

// Filenames the mill keeps its signing material under, inside a directory
// only its own account can read.
const (
	keyFile            = "signing_key"
	publicKeyFile      = "signing_key.pub"
	allowedSignersFile = "allowed_signers"
)

// Key is the mill's signing key and everything git needs to use it.
type Key struct {
	// Identity is the address the mill commits under, and the principal the
	// allowed_signers line names.
	Identity string
	// Name is the name on the commit's author and committer lines.
	Name string
	// Path is the private key. Never leaves the mill's account.
	Path string
	// PublicKey is the authorized_keys line for Path.
	PublicKey string
	// AllowedSigners is the file `git verify-commit` is verified against.
	AllowedSigners string
}

// Generate creates an ed25519 signing key in dir, with no passphrase,
// together with the allowed_signers file that verifies it.
//
// ssh-keygen does the work because git's SSH signing is itself ssh-keygen:
// a key this package wrote by hand would still be signed and verified by that
// binary, so the binary is a dependency either way. It is spawned through the
// workspace's execer like every other process the mill runs.
func Generate(ctx context.Context, execer workspace.Execer, dir, name, identity string) (*Key, error) {
	if identity == "" {
		return nil, fmt.Errorf("signing: no identity to generate a key for")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("signing: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, keyFile)
	if _, err := os.Stat(path); err == nil {
		return Load(dir, name, identity)
	}

	process, err := execer.Exec(ctx, workspace.Command{
		Path: "ssh-keygen",
		Args: []string{"-q", "-t", "ed25519", "-N", "", "-C", identity, "-f", path},
		Env:  map[string]string{},
	})
	if err != nil {
		return nil, fmt.Errorf("signing: ssh-keygen: %w", err)
	}
	if _, err := io.Copy(io.Discard, process.Stdout); err != nil {
		return nil, fmt.Errorf("signing: ssh-keygen: %w", err)
	}
	stderr, err := io.ReadAll(process.Stderr)
	if err != nil {
		return nil, fmt.Errorf("signing: ssh-keygen: %w", err)
	}
	if result := process.Wait(); result.Status != 0 {
		return nil, fmt.Errorf("signing: ssh-keygen exited %d: %s", result.Status, strings.TrimSpace(string(stderr)))
	}
	return Load(dir, name, identity)
}

// Load reads a signing key already in dir and writes the allowed_signers file
// that matches it.
func Load(dir, name, identity string) (*Key, error) {
	public, err := os.ReadFile(filepath.Join(dir, publicKeyFile))
	if err != nil {
		return nil, fmt.Errorf("signing: read public key: %w", err)
	}
	key := &Key{
		Identity:       identity,
		Name:           name,
		Path:           filepath.Join(dir, keyFile),
		PublicKey:      strings.TrimSpace(string(public)),
		AllowedSigners: filepath.Join(dir, allowedSignersFile),
	}
	if err := os.WriteFile(key.AllowedSigners, []byte(key.AllowedSignersLine()+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("signing: write allowed_signers: %w", err)
	}
	return key, nil
}

// AllowedSignersLine is the one line that trusts this key for git signatures
// made by this identity, and for nothing else.
func (k *Key) AllowedSignersLine() string {
	fields := strings.Fields(k.PublicKey)
	// The comment on an authorized_keys line is free text and would be read
	// as another field here, so only the type and the key itself are kept.
	if len(fields) > 2 {
		fields = fields[:2]
	}
	return fmt.Sprintf("%s namespaces=\"git\" %s", k.Identity, strings.Join(fields, " "))
}

// GitConfig is the `-c` arguments that make git sign with this key and verify
// against its allowed_signers. Passing them per invocation rather than
// writing a config file is what keeps the mill's identity out of the
// checkout, where a session could read or change it.
func (k *Key) GitConfig() []string {
	if k == nil {
		return nil
	}
	return []string{
		"-c", "gpg.format=ssh",
		"-c", "user.signingkey=" + k.Path,
		"-c", "gpg.ssh.allowedSignersFile=" + k.AllowedSigners,
		"-c", "commit.gpgsign=true",
		"-c", "user.name=" + k.Name,
		"-c", "user.email=" + k.Identity,
	}
}
