package local_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/forge/gitlab"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/local"
	"github.com/steading-ai/mill/internal/workspace/redact"
	"github.com/steading-ai/mill/internal/workspace/socket"
)

var _ = Describe("The local workspace's forge verbs", Label("unit"), func() {
	const (
		theBearer   = "the-live-step-token"
		theForgeKey = "glpat-the-step-alone-holds-this" // gitleaks:allow
		nodeRef     = "mill#13"
		branch      = "mill/mill-13"
	)

	var (
		root    string
		bare    string
		api     *httptest.Server
		squash  string
		log     *bytes.Buffer
		ws      *local.Workspace
		client  forge.Forge
		handle  *workspace.Handle
		session *socket.Client
		minted  int
	)

	BeforeEach(func(ctx context.Context) {
		root = filepath.Join(GinkgoT().TempDir(), "workspaces")
		log = &bytes.Buffer{}

		// The forge's repository, as a bare repository on disk: a real push
		// with a real ref to assert on, and no HTTP server pretending to be
		// git.
		bare = filepath.Join(GinkgoT().TempDir(), "thing.git")
		git(GinkgoT().TempDir(), "clone", "--quiet", "--bare", origin(), bare)

		// GitLab's API, answering about a merge request that was squash
		// merged into main. squash is the commit that landed there; it is
		// created below, on the bare repository's main.
		squash = squashOnMain(bare)
		api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests") && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`[]`))
			case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests") && r.Method == http.MethodPost:
				_, _ = w.Write([]byte(`{"iid":7,"state":"opened","web_url":"https://gitlab.example.test/group/thing/-/merge_requests/7",` +
					`"source_branch":"` + branch + `","target_branch":"main","squash":true}`))
			case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests/7"):
				_, _ = w.Write([]byte(`{"iid":7,"state":"merged","web_url":"https://gitlab.example.test/group/thing/-/merge_requests/7",` +
					`"source_branch":"` + branch + `","target_branch":"main","squash":true,"squash_commit_sha":"` + squash + `"}`))
			// The branch endpoint answers from the bare repository the
			// push actually goes to, so a merge request is opened only
			// after the ref is really there.
			case strings.Contains(r.URL.EscapedPath(), "/repository/branches/"):
				probe := exec.Command("git", "--git-dir="+bare, "rev-parse", "refs/heads/"+branch)
				probe.Env = testsupport.ScratchEnv()
				head, err := probe.Output()
				if err != nil {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"404 Branch Not Found"}`))
					return
				}
				_, _ = w.Write([]byte(`{"name":"` + branch + `","commit":{"id":"` + strings.TrimSpace(string(head)) + `"}}`))
			case strings.HasSuffix(r.URL.EscapedPath(), "/pipelines"):
				_, _ = w.Write([]byte(`[]`))
			default:
				_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/thing","default_branch":"main","squash_option":"default_on"}`))
			}
		}))
		DeferCleanup(api.Close)

		minted = 0
		client = gitlab.New(forge.Repo{
			Kind:     forge.KindGitLab,
			BaseURL:  api.URL,
			Path:     "group/thing",
			CloneURL: bare,
		}, forge.MinterFunc(func(context.Context, forge.Step) (forge.Credential, error) {
			minted++
			return forge.Credential{Username: "mill", Token: theForgeKey}, nil
		}), gitlab.WithHTTPClient(api.Client()))

		ws = local.New(local.Options{
			Root:     root,
			Origin:   bare,
			Log:      log,
			Redactor: redact.New(),
			Identity: "mill@example.test",
			Name:     "mill",
			Forge:    client,
		})

		made, err := ws.Create(ctx, workspace.Spec{RunID: "01J0RUN13", NodeRef: nodeRef, Origin: bare})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		handle = made

		Expect(ws.Rotate(ctx, workspace.TokenFor(theBearer, workspace.Read, workspace.Write, workspace.Exec, workspace.Forge))).To(Succeed())
		session = socket.Dial(handle.Socket, theBearer)
	})

	// commit puts one commit on the run's branch through the workspace, which
	// is the only way a session may write history.
	commit := func(ctx context.Context) string {
		GinkgoHelper()
		Expect(session.Write(ctx, "NOTES.md", []byte("worked\n"))).To(Succeed())
		made, err := session.GitCommit(ctx, workspace.CommitRequest{Message: "mill#13: worked"})
		Expect(err).NotTo(HaveOccurred())
		return made.SHA
	}

	It("pushes the run's branch with a credential the session never saw", func(ctx context.Context) {
		head := commit(ctx)

		pushed, err := session.Push(ctx, workspace.PushRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pushed.Branch).To(Equal(branch))
		Expect(pushed.SHA).To(Equal(head))
		Expect(minted).To(BeNumerically(">", 0), "the push minted a credential of its own")

		Expect(git(bare, "rev-parse", "refs/heads/"+branch)).To(Equal(head),
			"the run's branch is on the forge, at the commit the run made")

		// The remote in the clone is the plain repository, with no token in
		// it, and nothing under .git holds one either.
		Expect(git(handle.Root, "remote", "-v")).NotTo(ContainSubstring(theForgeKey))
		Expect(git(handle.Root, "config", "--get", "remote.origin.url")).NotTo(ContainSubstring(theForgeKey))
		Expect(grepTree(filepath.Dir(handle.Root), theForgeKey)).To(BeEmpty(),
			"the credential was never written anywhere in the workspace")

		// Nor did the session's own environment ever carry it.
		env, err := session.Exec(ctx, socket.ExecRequest{Path: "/bin/sh", Args: []string{"-c", "env"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(env.Stdout).NotTo(ContainSubstring(theForgeKey))
		Expect(log.String()).NotTo(ContainSubstring(theForgeKey), "nor a log the mill wrote")
	})

	It("opens the run branch's pull request, and returns the same one when asked again", func(ctx context.Context) {
		commit(ctx)
		_, err := session.Push(ctx, workspace.PushRequest{})
		Expect(err).NotTo(HaveOccurred())

		pr, err := session.PR(ctx, workspace.PRRequest{Title: "mill#13: the forge adapter"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.URL).To(Equal("https://gitlab.example.test/group/thing/-/merge_requests/7"))
		Expect(pr.Branch).To(Equal(branch), "the branch is the run's, not the session's to name")
	})

	It("reports success from a project that configures no pipeline", func(ctx context.Context) {
		head := commit(ctx)

		checks, err := session.Checks(ctx, workspace.ChecksRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks.SHA).To(Equal(head), "the run branch's head, by default")
		Expect(checks.Status).To(Equal(forge.Success))
	})

	It("resolves the merge commit in the checkout after a fetch", func(ctx context.Context) {
		head := commit(ctx)
		_, err := session.Push(ctx, workspace.PushRequest{})
		Expect(err).NotTo(HaveOccurred())

		// The mill reads the merge commit itself: merging is its own act at
		// the end of a run and has no verb on the socket.
		landed, err := client.MergeCommit(ctx, forge.PullRequest{Number: 7})
		Expect(err).NotTo(HaveOccurred())
		Expect(landed).NotTo(Equal(head), "the squash commit is not the branch's head")

		git(handle.Root, "fetch", "--quiet", "origin")
		Expect(git(handle.Root, "cat-file", "-t", landed)).To(Equal("commit"))
		Expect(git(handle.Root, "merge-base", "--is-ancestor", landed, "origin/main")).To(BeEmpty(),
			"the merge commit is on the default branch")
	})

	It("refuses the forge verbs on a run whose project has no repository", func(ctx context.Context) {
		unattached := local.New(local.Options{
			Root:     filepath.Join(GinkgoT().TempDir(), "no-forge"),
			Origin:   origin(),
			Redactor: redact.New(),
			Identity: "mill@example.test",
		})
		made, err := unattached.Create(ctx, workspace.Spec{RunID: "01J0RUNX", NodeRef: nodeRef})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(unattached.Destroy(ctx)).To(Succeed()) })
		Expect(unattached.Rotate(ctx, workspace.TokenFor("another-token", workspace.Forge))).To(Succeed())

		_, err = socket.Dial(made.Socket, "another-token").PR(ctx, workspace.PRRequest{})
		var wire *socket.Error
		Expect(errors.As(err, &wire)).To(BeTrue(), "%v", err)
		Expect(wire.Code).To(Equal(socket.CodeNotImplemented))
		Expect(wire.Message).To(ContainSubstring("no forge"))
	})
})

// squashOnMain puts a commit on the bare repository's main branch, standing
// in for what GitLab writes when it squash merges a merge request: a commit
// on the default branch that exists on no branch the run pushed.
func squashOnMain(bare string) string {
	GinkgoHelper()
	work := filepath.Join(GinkgoT().TempDir(), "forge-side")
	git(GinkgoT().TempDir(), "clone", "--quiet", bare, work)
	Expect(os.WriteFile(filepath.Join(work, "SQUASHED.md"), []byte("squashed\n"), 0o600)).To(Succeed())
	git(work, "add", "SQUASHED.md")
	git(work, "commit", "--quiet", "--no-gpg-sign", "--message", "squashed: mill#13")
	git(work, "push", "--quiet", "origin", "main")
	return git(work, "rev-parse", "HEAD")
}

// grepTree is every path under dir whose bytes contain value. It is how a
// spec asserts that a credential reached no file.
func grepTree(dir, value string) []string {
	GinkgoHelper()
	var found []string
	Expect(filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // an unreadable entry holds nothing the spec can assert on
		}
		content, err := os.ReadFile(path) //nolint:gosec // a path this spec created
		if err != nil {
			return nil //nolint:nilerr // likewise
		}
		if bytes.Contains(content, []byte(value)) {
			found = append(found, path)
		}
		return nil
	})).To(Succeed())
	return found
}

var _ = Describe("A workspace on a private origin", Label("unit"), func() {
	const (
		theBearer   = "the-live-step-token"
		theForgeKey = "glpat-only-the-workspace-holds-this" // gitleaks:allow
		theUser     = "mill"
	)

	var (
		remote string
		log    *bytes.Buffer
		ws     *local.Workspace
	)

	BeforeEach(func() {
		// Given a forge origin that refuses anonymous reads: a real
		// repository served over git's smart HTTP protocol, behind basic
		// auth, with nothing on the host that could answer for it.
		remote = privateOrigin(theUser, theForgeKey)
		log = &bytes.Buffer{}
		ws = local.New(local.Options{
			Root:     filepath.Join(GinkgoT().TempDir(), "workspaces"),
			Origin:   remote,
			Log:      log,
			Redactor: redact.New(),
			Identity: "mill@example.test",
			Name:     "mill",
			Forge: gitlab.New(
				forge.Repo{Kind: forge.KindGitLab, Path: "group/thing", CloneURL: remote},
				forge.Static(theUser, theForgeKey),
			),
		})
	})

	It("A private origin clones with the forge's credential", func(ctx context.Context) {
		// Given a clone with no credential and no helper to answer for one
		// — the host's own git configuration is out of reach, as it is for
		// the workspace — is refused
		anonymous := exec.CommandContext(ctx, "git", "clone", "--quiet", remote,
			filepath.Join(GinkgoT().TempDir(), "anonymous"))
		anonymous.Env = testsupport.MinimalEnv(testsupport.SpecGit("GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")...)
		refused, err := anonymous.CombinedOutput()
		Expect(err).To(HaveOccurred(), "the origin let an anonymous read through")
		Expect(string(refused)).To(ContainSubstring("Authentication failed"))

		// When the workspace is created for a run on that origin
		handle, err := ws.Create(ctx, workspace.Spec{RunID: "01J0RUN44", NodeRef: "mill#44", Origin: remote})

		// Then the clone succeeds
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		Expect(os.ReadFile(filepath.Join(handle.Root, "README.md"))).To(Equal([]byte("as cloned\n")))
		Expect(handle.Branch).To(Equal("mill/mill-44"))

		// And the session's environment carries no credential
		Expect(ws.Rotate(ctx, workspace.TokenFor(theBearer, workspace.Exec))).To(Succeed())
		session := socket.Dial(handle.Socket, theBearer)
		env, err := session.Exec(ctx, socket.ExecRequest{Path: "/usr/bin/env"})
		Expect(err).NotTo(HaveOccurred())
		Expect(env.Stdout).NotTo(ContainSubstring(theForgeKey))
		Expect(lines(env.Stdout)).To(ConsistOf(floorAndGrant(ws, handle.Socket, theBearer)))

		// Nor did it reach the checkout, or a log the mill wrote.
		Expect(grepTree(filepath.Dir(handle.Root), theForgeKey)).To(BeEmpty())
		Expect(log.String()).NotTo(ContainSubstring(theForgeKey))
	})

	It("presents the credential to the forge's own repository and to no other origin", func(ctx context.Context) {
		// A workspace pointed by --origin at a repository the forge does not
		// serve gets no credential: the mill's forge token is for the
		// mill's forge.
		elsewhere := local.New(local.Options{
			Root:     filepath.Join(GinkgoT().TempDir(), "elsewhere"),
			Redactor: redact.New(),
			Identity: "mill@example.test",
			Forge: gitlab.New(
				forge.Repo{Kind: forge.KindGitLab, Path: "group/other", CloneURL: "https://gitlab.example.test/group/other.git"},
				forge.Static(theUser, theForgeKey),
			),
		})

		_, err := elsewhere.Create(ctx, workspace.Spec{RunID: "01J0RUN45", NodeRef: "mill#44", Origin: remote})

		Expect(err).To(MatchError(ContainSubstring("git clone exited")))
	})
})

// privateOrigin is a repository served over git's smart HTTP protocol, behind
// basic auth: `git http-backend` as a CGI program, which is how git itself is
// served, wrapped in a handler that answers 401 to anyone who does not
// present the credential.
func privateOrigin(username, password string) string {
	GinkgoHelper()

	root := GinkgoT().TempDir()
	git(GinkgoT().TempDir(), "clone", "--quiet", "--bare", origin(), filepath.Join(root, "thing.git"))

	binary, err := exec.LookPath("git")
	Expect(err).NotTo(HaveOccurred())
	backend := &cgi.Handler{
		Path: binary,
		Args: []string{"http-backend"},
		Dir:  root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"HOME=" + root,
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull,
			"LC_ALL=C",
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != username || secret != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	DeferCleanup(server.Close)
	return server.URL + "/thing.git"
}
