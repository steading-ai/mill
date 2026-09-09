package acceptance_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The variables the suite reads. Every one of them is documented in
// docs/runbook.md, and the preflight names the ones that are missing rather
// than letting a run fail four minutes in.
const (
	// EnabledVar turns the suite on. Unset, every spec skips.
	EnabledVar = "MILL_ACCEPTANCE"

	// ForgeURLVar is the GitLab the scratch project is made on. Unset, the
	// suite runs in local-origin mode: the scenarios that deliver to a
	// forge skip, and the ones that end before a run reaches one — the
	// seeded bad implementation — run against a bare repository on disk.
	ForgeURLVar = "MILL_ACCEPTANCE_FORGE_URL"
	// ForgeTokenVar is a token with `api` scope in the scratch namespace.
	ForgeTokenVar = "MILL_ACCEPTANCE_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
	// ForgeNamespaceVar is the group scratch projects are created in and
	// deleted from, by numeric id or by full path.
	ForgeNamespaceVar = "MILL_ACCEPTANCE_FORGE_NAMESPACE"
	// ForgeUserVar is the basic-auth username GitLab wants beside the token.
	ForgeUserVar = "MILL_ACCEPTANCE_FORGE_USER"

	// DatabaseURLVar is the PostgreSQL the suite's own `mill serve` owns.
	DatabaseURLVar = "MILL_ACCEPTANCE_DATABASE_URL"

	// DaggerSocketVar is the deployed dagger the suite files its nodes in.
	DaggerSocketVar = "MILL_ACCEPTANCE_DAGGER_SOCKET"
	// SecondDaggerConfigVar is a `dg --config` for a second principal, used
	// by the fencing scenario to take the node's lease away from the mill.
	// Optional: without it the scenario claims as the same principal under a
	// second agent ref, which fences the running worker just the same.
	SecondDaggerConfigVar = "MILL_ACCEPTANCE_SECOND_DAGGER_CONFIG"

	// credentialsPath is where the account's Claude Code login lives,
	// relative to $HOME. It is what the sidecar's sessions run on: a key or
	// an OAuth token in the environment is on the harness adapter's deny
	// list and never reaches a sidecar (docs/architecture.md §"Skill steps:
	// the harness adapter").
	credentialsPath = ".claude/.credentials.json"
)

// defaultDaggerSocket is where dagger listens when nothing says otherwise.
const defaultDaggerSocket = "/run/dagger/dg.sock"

// Environment is everything the suite was given, resolved once.
type Environment struct {
	// RepoRoot is this checkout, which the workflow directory and the
	// sidecar are read from.
	RepoRoot string
	// Mill is the binary under test, built by the suite.
	Mill string
	// Sidecar is the harness entrypoint, absolute because a session's
	// working directory is some other project's checkout.
	Sidecar string
	// DG is dagger's CLI.
	DG string

	ForgeURL       string
	ForgeToken     string
	ForgeNamespace string
	ForgeUser      string

	DatabaseURL string

	DaggerSocket       string
	SecondDaggerConfig string
}

// env is resolved by the preflight and read by every spec.
var env Environment

// enabled reports whether the suite was asked for at all.
func enabled() bool { return os.Getenv(EnabledVar) != "" }

// preflight resolves the environment and refuses the whole suite when
// anything it needs is absent, naming everything that is absent in one
// message. A suite that spends a merge request and a Claude session before
// discovering it has no forge token has already cost more than the message
// is worth.
func preflight() Environment {
	GinkgoHelper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	Expect(err).NotTo(HaveOccurred())

	resolved := Environment{
		RepoRoot:           root,
		Sidecar:            filepath.Join(root, "sidecar", "dist", "main.js"),
		ForgeURL:           strings.TrimSuffix(os.Getenv(ForgeURLVar), "/"),
		ForgeToken:         os.Getenv(ForgeTokenVar),
		ForgeNamespace:     os.Getenv(ForgeNamespaceVar),
		ForgeUser:          valueOr(os.Getenv(ForgeUserVar), "mill"),
		DatabaseURL:        os.Getenv(DatabaseURLVar),
		DaggerSocket:       valueOr(os.Getenv(DaggerSocketVar), valueOr(os.Getenv("DG_SOCKET"), defaultDaggerSocket)),
		SecondDaggerConfig: os.Getenv(SecondDaggerConfigVar),
	}

	var missing []string
	required := []struct{ name, value string }{{DatabaseURLVar, resolved.DatabaseURL}}
	// A forge is optional, and half a forge is not: a URL with no token or
	// no namespace is a configuration mistake, and skipping the scenarios
	// that need one would hide it.
	if resolved.ForgeURL != "" {
		required = append(required,
			struct{ name, value string }{ForgeTokenVar, resolved.ForgeToken},
			struct{ name, value string }{ForgeNamespaceVar, resolved.ForgeNamespace})
	}
	for _, want := range required {
		if want.value == "" {
			missing = append(missing, want.name+" is unset")
		}
	}
	if home, err := os.UserHomeDir(); err != nil {
		missing = append(missing, "no home directory to find the Claude login in: "+err.Error())
	} else if _, err := os.Stat(filepath.Join(home, credentialsPath)); err != nil {
		missing = append(missing, "no Claude login at ~/"+credentialsPath+"; the sessions run on the account's own login, never on a credential in the environment")
	}
	if _, err := os.Stat(resolved.DaggerSocket); err != nil {
		missing = append(missing, "nothing is listening on "+resolved.DaggerSocket+" ("+DaggerSocketVar+")")
	}
	if _, err := os.Stat(resolved.Sidecar); err != nil {
		missing = append(missing, resolved.Sidecar+" does not exist; run `make sidecar-build`")
	}
	if resolved.SecondDaggerConfig != "" {
		if _, err := os.Stat(resolved.SecondDaggerConfig); err != nil {
			missing = append(missing, resolved.SecondDaggerConfig+" does not exist ("+SecondDaggerConfigVar+")")
		}
	}
	for _, tool := range []struct {
		name string
		into *string
	}{
		{"dg", &resolved.DG},
		{"git", nil},
		{"node", nil},
		{"go", nil},
	} {
		path, err := exec.LookPath(tool.name)
		if err != nil {
			missing = append(missing, tool.name+" is not on PATH")
			continue
		}
		if tool.into != nil {
			*tool.into = path
		}
	}

	if len(missing) > 0 {
		Fail(fmt.Sprintf("%s is set, but the acceptance suite cannot start:\n  - %s\n\nSee docs/runbook.md for what each of these is and how to supply it.",
			EnabledVar, strings.Join(missing, "\n  - ")))
	}
	return resolved
}

// forgeConfigured reports whether the suite was given a forge to deliver to.
// Without one every scenario that ends in a merge request skips, and the
// scenarios that end before a run reaches a forge run against a bare
// repository on disk (docs/runbook.md §"Without a forge").
func forgeConfigured() bool { return env.ForgeURL != "" }

// needsForge skips the calling spec when the suite has no forge.
func needsForge() {
	GinkgoHelper()
	if !forgeConfigured() {
		Skip("set " + ForgeURLVar + " to run the scenarios that deliver to a forge; see docs/runbook.md")
	}
}

// valueOr is a value or a fallback.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
