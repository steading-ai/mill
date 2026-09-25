package acceptance_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
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

	// SkiftSocketVar is an external skift to file nodes in, for the
	// deliberate case of exercising the deployed service. Unset, the suite
	// starts a skift of its own from internal/testsupport and files there,
	// so nothing it makes reaches a graph anyone reads. It is the only
	// variable that points the suite at a skift: `SKIFT_SOCKET` in the
	// operator's shell names the deployed one and is ignored here on
	// purpose.
	SkiftSocketVar = "MILL_ACCEPTANCE_SKIFT_SOCKET"
	// SecondSkiftConfigVar is a `skift --config` for a second principal, used
	// by the fencing scenario to take the node's lease away from the mill.
	// Optional: without it the scenario claims as the same principal under a
	// second agent ref, which fences the running worker just the same. It
	// names a principal of the external skift, so it is read only when
	// SkiftSocketVar is set.
	SecondSkiftConfigVar = "MILL_ACCEPTANCE_SECOND_SKIFT_CONFIG"

	// credentialsPath is where the account's Claude Code login lives,
	// relative to $HOME. It is what the sidecar's sessions run on: a key or
	// an OAuth token in the environment is on the harness adapter's deny
	// list and never reaches a sidecar (docs/architecture.md §"Skill steps:
	// the harness adapter").
	credentialsPath = ".claude/.credentials.json"
)

// deployedSkiftSocket is where the operator's skift listens. The suite
// never files there unless SkiftSocketVar says so; the two scenarios about
// the suite's own skift read its project list to prove that.
const deployedSkiftSocket = "/run/skift/skift.sock"

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
	// Skift is skift's CLI.
	Skift string

	ForgeURL       string
	ForgeToken     string
	ForgeNamespace string
	ForgeUser      string

	DatabaseURL string

	// SkiftSocket is the skift every scratch project is created in. It
	// is SkiftSocketVar when that is set, and otherwise the socket of the
	// skift the suite started for itself, filled in by the BeforeSuite.
	SkiftSocket string
	// ExternalSkift reports whether SkiftSocket was named by the
	// environment rather than started by the suite.
	ExternalSkift     bool
	SecondSkiftConfig string
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
		RepoRoot:          root,
		Sidecar:           filepath.Join(root, "sidecar", "dist", "main.js"),
		ForgeURL:          strings.TrimSuffix(os.Getenv(ForgeURLVar), "/"),
		ForgeToken:        os.Getenv(ForgeTokenVar),
		ForgeNamespace:    os.Getenv(ForgeNamespaceVar),
		ForgeUser:         valueOr(os.Getenv(ForgeUserVar), "mill"),
		DatabaseURL:       os.Getenv(DatabaseURLVar),
		SkiftSocket:       os.Getenv(SkiftSocketVar),
		ExternalSkift:     os.Getenv(SkiftSocketVar) != "",
		SecondSkiftConfig: os.Getenv(SecondSkiftConfigVar),
	}
	if !resolved.ExternalSkift {
		// A second principal is one the external skift knows. The suite's
		// own skift seeds one principal, this process's uid, and the
		// fencing scenario claims as it under a second agent ref.
		resolved.SecondSkiftConfig = ""
	}

	var missing []string
	for _, want := range required(resolved) {
		if want.value == "" {
			missing = append(missing, want.name+" is unset")
		}
	}
	if home, err := os.UserHomeDir(); err != nil {
		missing = append(missing, "no home directory to find the Claude login in: "+err.Error())
	} else if _, err := os.Stat(filepath.Join(home, credentialsPath)); err != nil {
		missing = append(missing, "no Claude login at ~/"+credentialsPath+"; the sessions run on the account's own login, never on a credential in the environment")
	}
	if resolved.ExternalSkift {
		if _, err := os.Stat(resolved.SkiftSocket); err != nil {
			missing = append(missing, "nothing is listening on "+resolved.SkiftSocket+" ("+SkiftSocketVar+")")
		}
	} else if reason := testsupport.SkiftUnavailable(); reason != "" {
		missing = append(missing, reason+"; or set "+SkiftSocketVar+" to an external skift")
	}
	if _, err := os.Stat(resolved.Sidecar); err != nil {
		missing = append(missing, resolved.Sidecar+" does not exist; run `make sidecar-build`")
	}
	if resolved.SecondSkiftConfig != "" {
		if _, err := os.Stat(resolved.SecondSkiftConfig); err != nil {
			missing = append(missing, resolved.SecondSkiftConfig+" does not exist ("+SecondSkiftConfigVar+")")
		}
	}
	for _, tool := range []struct {
		name string
		into *string
	}{
		{"skift", &resolved.Skift},
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

// variable is one thing the environment must carry, and what it carries.
type variable struct{ name, value string }

// required is what an environment must have for the suite to start.
//
// A forge is optional, and half a forge is not: a URL with no token or no
// namespace is a configuration mistake, and skipping the scenarios that
// need one would hide it. Everything else here is needed by every scenario
// there is.
func required(resolved Environment) []variable {
	wanted := []variable{{DatabaseURLVar, resolved.DatabaseURL}}
	if resolved.ForgeURL != "" {
		wanted = append(wanted,
			variable{ForgeTokenVar, resolved.ForgeToken},
			variable{ForgeNamespaceVar, resolved.ForgeNamespace})
	}
	return wanted
}

// forgeConfigured reports whether the suite was given a forge to deliver to.
// Without one every scenario that ends in a merge request skips, and the
// scenarios that end before a run reaches a forge run against a bare
// repository on disk (docs/runbook.md §"Without a forge").
func forgeConfigured() bool { return env.ForgeURL != "" }

// needsForge skips the calling spec when the suite has no forge.
func needsForge() {
	GinkgoHelper()
	if reason := forgeSkipReason(forgeConfigured()); reason != "" {
		Skip(reason)
	}
}

// forgeSkipReason is why a forge-dependent scenario did not run, or empty
// when it can. A skipped scenario is a scenario nobody proved, so the
// message names the variable that would have run it and where to read about
// it: a suite that quietly ran four of its five scenarios would be a suite
// that lied about the milestone.
func forgeSkipReason(configured bool) string {
	if configured {
		return ""
	}
	return "set " + ForgeURLVar + " to run the scenarios that deliver to a forge; see docs/runbook.md"
}

// valueOr is a value or a fallback.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: The suite is honest about what it could not run
	//   Given no forge is configured
	//   When the suite runs
	//   Then forge-dependent scenarios are skipped naming MILL_ACCEPTANCE_FORGE_URL
	//   And the reviewer scenario runs
	It("The suite is honest about what it could not run", func(ctx SpecContext) {
		By("given no forge is configured")
		// The environment a host with no GitLab resolves to. It starts the
		// suite — a forge is optional — and it is the input every decision
		// below is made from.
		unconfigured := Environment{DatabaseURL: env.DatabaseURL}
		Expect(unconfigured.ForgeURL).To(BeEmpty())
		Expect(required(unconfigured)).To(ConsistOf(variable{DatabaseURLVar, env.DatabaseURL}),
			"a host with no forge is asked for no forge credentials")

		By("when the suite runs")
		reason := forgeSkipReason(unconfigured.ForgeURL != "")

		By("then forge-dependent scenarios are skipped naming MILL_ACCEPTANCE_FORGE_URL")
		Expect(ForgeURLVar).To(Equal("MILL_ACCEPTANCE_FORGE_URL"))
		Expect(reason).To(SatisfyAll(
			ContainSubstring(ForgeURLVar),
			ContainSubstring("docs/runbook.md")), "the skip says which variable would have run it")
		Expect(forgeSkipReason(true)).To(BeEmpty(), "a configured forge skips nothing")

		By("and the reviewer scenario runs")
		// It is the milestone's own proof and it must not depend on an
		// account not every host has, so with no forge it stages a bare
		// repository on this host and clones it with real git. Staged here
		// as that scenario stages it, and asserted to have reached for
		// nothing the unconfigured environment lacks.
		s := reviewerFixture(false).stage(ctx)
		Expect(s.Project).To(BeNil(), "no scratch project on a forge")
		Expect(s.Origin).To(HavePrefix("file://"), "a repository on this host")
		Expect(s.VCSURL).To(Equal(placeholderVCSURL))
		Expect(s.NodeRef).NotTo(BeEmpty(), "the node is filed and the scenario can run")
	})
})
