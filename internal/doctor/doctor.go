// Package doctor holds the probe logic that only the process actually
// spawning the sidecar, holding the signing key, and delivering to the
// forge can answer honestly. internal/cli's `mill doctor` calls it against
// its own local state; a client host reaching a worker over a forwarded
// socket instead gets these answers from the worker's own process, through
// internal/server's WorkerService, which calls the same functions.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/harness/claude"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/local"
	"github.com/steading-ai/mill/internal/workspace/signing"
)

// ProbeTimeout bounds one probe, wherever it runs: a probe that hangs is a
// failed probe, not a slow one.
const ProbeTimeout = 10 * time.Second

// allowedSignersNames are the two names an allowed_signers file is looked
// for under, a checkout's or the project's repository's.
var allowedSignersNames = []string{"allowed_signers", ".allowed_signers"}

// Result is one probe's answer: pass or fail, and the one line a human
// reads either way.
type Result struct {
	OK     bool
	Reason string
}

func pass(format string, args ...any) Result {
	return Result{OK: true, Reason: fmt.Sprintf(format, args...)}
}

func fail(format string, args ...any) Result {
	return Result{OK: false, Reason: fmt.Sprintf(format, args...)}
}

// Sidecar starts the harness sidecar and asks it for a no-op. Nothing short
// of running it proves it runs: the entrypoint may be missing, node may be
// absent, the bundle may be stale.
//
// It is spawned under the environment mode a run would spawn it with, and
// the line says which — a sidecar that starts under `inherit` and not under
// `strict` is a mode away from a run that cannot start a session, and the
// probe is where that difference is meant to show up.
func Sidecar(ctx context.Context, entrypoint, envMode string) Result {
	mode, err := claude.ParseEnvMode(envMode)
	if err != nil {
		return fail("%v", err)
	}
	adapter := claude.New(claude.WithEntrypoint(claude.DefaultCommand, entrypoint), claude.WithEnvMode(mode))
	terminal, err := adapter.Probe(ctx)
	if err != nil {
		return fail("%s did not answer under environment mode %s: %v", entrypoint, mode, err)
	}
	return pass("%s starts under environment mode %s and answers: %s",
		entrypoint, mode, firstLine(terminal.FinalMessage))
}

// firstLine is a message trimmed to one line, for a probe's reason.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if line == "" {
		return "(no message)"
	}
	return line
}

// Signing finds the mill's signing key and looks for its public key in an
// allowed_signers file of a project the mill has checked out. A key nothing
// trusts signs commits nobody can verify, which is the failure this probe
// exists to catch (docs/architecture.md §"Identity and credentials").
//
// A fresh host has a key and no checkout yet — nothing for the checkout
// scan to find. When source is not nil, a checkout naming nothing trusted
// falls back to asking the project's repository directly for the same
// file, so such a host can be green before its first run.
func Signing(ctx context.Context, workspaceDir, name, identity string, source forge.ContentReader) Result {
	dir := filepath.Join(workspaceDir, "signing")
	key, err := signing.Load(dir, name, identity)
	if err != nil {
		return fail("no signing key under %s: %v", dir, err)
	}
	blob := keyBlob(key.PublicKey)
	trusted, err := trustedBy(workspaceDir, blob)
	if err != nil {
		return fail("key %s: %v", identity, err)
	}
	if len(trusted) > 0 {
		return pass("key %s is present and trusted by %s", identity, strings.Join(trusted, ", "))
	}
	if source != nil {
		ok, err := trustedByRepository(ctx, source, blob)
		if err != nil {
			return fail("key %s: %v", identity, err)
		}
		if ok {
			return pass("key %s is present and trusted by the project's repository", identity)
		}
	}
	return fail("key %s is present but no project's allowed_signers names it", identity)
}

// trustedByRepository asks the project's repository directly for an
// allowed_signers file, the way trustedBy asks a checkout — for a host with
// a key and no checkout to scan yet.
func trustedByRepository(ctx context.Context, source forge.ContentReader, blob string) (bool, error) {
	for _, name := range allowedSignersNames {
		content, err := source.ReadFile(ctx, name)
		if errors.Is(err, forge.ErrNoSuchFile) {
			continue
		}
		if err != nil {
			return false, err
		}
		return blob != "" && strings.Contains(content, blob), nil
	}
	return false, nil
}

// trustedBy is the projects whose checkout carries an allowed_signers file
// naming the key. Checkouts are the only copy of a project's repository this
// host has, so they are where the question is asked.
func trustedBy(workspaceDir, blob string) ([]string, error) {
	entries, err := os.ReadDir(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", workspaceDir, err)
	}
	var trusted []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "signing" {
			continue
		}
		for _, name := range allowedSignersNames {
			path := filepath.Join(workspaceDir, entry.Name(), "checkout", name)
			raw, err := os.ReadFile(path) //nolint:gosec // a path under the worker's own workspace directory
			if err != nil {
				continue
			}
			if blob != "" && strings.Contains(string(raw), blob) {
				trusted = append(trusted, entry.Name())
				break
			}
		}
	}
	return trusted, nil
}

// keyBlob is the base64 body of an SSH public key: the part an
// allowed_signers line must carry, without the comment that follows it.
func keyBlob(publicKey string) string {
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// Forge asks the resolved forge whether the credential a caller already
// loaded will serve a delivery, before a run spends a session finding out.
// The caller has already resolved the project's repository and the forge
// adapter — internal/cli/doctor.go against its own configuration,
// internal/server's WorkerService against the worker's — so this holds only
// the part both share: what to do with the answer.
//
// A forge that can say who the credential is (forge.Prober) is asked, and
// the account it names is on the line; one that cannot is asked about a
// branch nothing has ever pushed, and "no pull request" is it answering.
// Every line names where the credential came from: a passing one so an
// operator can see the store is in use, a failing one because the
// credential is what to replace.
func Forge(ctx context.Context, f forge.Forge, tokenSource, repositoryURL string) Result {
	if prober, ok := f.(forge.Prober); ok {
		account, err := prober.Probe(ctx)
		if err != nil {
			return fail("%s refused %s: %v", repositoryURL, tokenSource, err)
		}
		return pass("%s accepts %s as %s", repositoryURL, tokenSource, account.Login)
	}
	_, err := f.PR(ctx, "mill-doctor-probe")
	if err == nil || errors.Is(err, forge.ErrNoPullRequest) {
		return pass("%s accepts %s", repositoryURL, tokenSource)
	}
	return fail("%s refused %s: %v", repositoryURL, tokenSource, err)
}

// SessionSockets asks a throwaway local workspace for the environment a
// session's tools would run under, and checks that SKIFT_SOCKET and MILL_SOCKET
// in it are dead: this is the wiring the mill#38 incident found missing,
// where a session ran unsandboxed against the production skift and mill
// sockets because nothing had ever overwritten them (docs/sandbox.md
// §Credentials).
//
// A workspace made with [local.Workspace.Create] cannot serve this probe: it
// requires an origin to clone, which a `mill doctor` invocation has none of.
// [local.Workspace.Attach] has no such precondition, so the probe provisions
// just enough of a checkout for it to succeed — an empty repository, never
// cloned from anywhere — under a fresh, real child of workspaceDir, so a
// workspace directory this host cannot actually use for a run (wrong
// filesystem, no exec permission) fails here too. The workspace is destroyed
// before this returns either way.
func SessionSockets(ctx context.Context, workspaceDir, socket, skiftSocket string) Result {
	dir, err := os.MkdirTemp(workspaceDir, "doctor-isolation-*")
	if err != nil {
		return fail("workspace directory %s: %v", workspaceDir, err)
	}
	checkout := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return fail("workspace directory %s: %v", workspaceDir, err)
	}
	if err := gitInit(ctx, checkout); err != nil {
		_ = os.RemoveAll(dir)
		return fail("git init %s: %v", checkout, err)
	}
	ws := local.New(local.Options{Root: workspaceDir})
	if _, err := ws.Attach(ctx, workspace.Spec{RunID: filepath.Base(dir), NodeRef: "doctor/isolation"}); err != nil {
		_ = os.RemoveAll(dir)
		return fail("attach a throwaway workspace under %s: %v", workspaceDir, err)
	}
	env := ws.Environment()
	_ = ws.Destroy(ctx)
	return checkSessionEnv(env, socket, skiftSocket)
}

// gitInit makes dir an empty git repository, spawned through
// [workspace.Spawner] like every other process the mill runs (os/exec is
// forbidden outside internal/workspace).
func gitInit(ctx context.Context, dir string) error {
	process, err := (workspace.Spawner{}).Exec(ctx, workspace.Command{
		Path: "git", Args: []string{"init", "--quiet", dir},
	})
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, process.Stdout); err != nil {
		return err
	}
	stderr, err := io.ReadAll(process.Stderr)
	if err != nil {
		return err
	}
	if result := process.Wait(); result.Status != 0 {
		return fmt.Errorf("exited %d: %s", result.Status, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// checkSessionEnv is whether a session's environment keeps SKIFT_SOCKET and
// MILL_SOCKET away from the sockets this host actually serves: present,
// distinct from socket and skiftSocket, and answering no dial. A dial
// succeeding on the value handed back means the wiring leaked the real
// address rather than a dead one — on a healthy host the real socket is
// live, which is what the `socket` probe already establishes.
func checkSessionEnv(env map[string]string, socket, skiftSocket string) Result {
	for name, real := range map[string]string{"SKIFT_SOCKET": skiftSocket, "MILL_SOCKET": socket} {
		value := env[name]
		if value == "" {
			return fail("%s is missing from a session's own environment", name)
		}
		if value == real {
			return fail("%s is %s, the socket this host actually serves", name, real)
		}
		if dialable(value) {
			return fail("%s is %s, which answers a dial", name, value)
		}
	}
	return pass("SKIFT_SOCKET and MILL_SOCKET are dead paths, neither %s nor %s", skiftSocket, socket)
}

// dialable reports whether something is listening on a unix socket path.
func dialable(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
