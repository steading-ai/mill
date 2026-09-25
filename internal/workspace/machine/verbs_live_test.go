package machine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/forge/gitlab"
	"github.com/steading-ai/mill/internal/sweep"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/signing"
)

// The forge half of the live tier, as the acceptance suite names it
// (docs/runbook.md): the GitLab scratch projects are made on, the token
// that may create and delete them, and the group they live in. Unset, the
// scenario that spends a scratch project skips and the rest of the tier
// runs against an origin on disk.
const (
	forgeURLVar       = "MILL_ACCEPTANCE_FORGE_URL"
	forgeTokenVar     = "MILL_ACCEPTANCE_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
	forgeNamespaceVar = "MILL_ACCEPTANCE_FORGE_NAMESPACE"
	forgeUserVar      = "MILL_ACCEPTANCE_FORGE_USER"
)

// scratchAPI is GitLab's v4 API with the suite's token, for making the
// scratch project a scenario delivers to and reading what landed there.
type scratchAPI struct {
	url, token, namespace, user string
}

// scratchProject is what the forge says about a scratch project.
type scratchProject struct {
	ID            int    `json:"id"`
	Name          string `json:"path"`
	Path          string `json:"path_with_namespace"`
	CloneURL      string `json:"http_url_to_repo"`
	DefaultBranch string `json:"default_branch"`
}

// scratchForge is the API, or a skip naming what the scenario needs.
func scratchForge() scratchAPI {
	GinkgoHelper()
	api := scratchAPI{
		url:       strings.TrimSuffix(os.Getenv(forgeURLVar), "/"),
		token:     os.Getenv(forgeTokenVar),
		namespace: os.Getenv(forgeNamespaceVar),
		user:      os.Getenv(forgeUserVar),
	}
	if api.url == "" || api.token == "" || api.namespace == "" {
		Skip("set " + forgeURLVar + ", " + forgeTokenVar + " and " + forgeNamespaceVar +
			" to run the spec that spends a scratch project on the forge (docs/runbook.md)")
	}
	if api.user == "" {
		api.user = "mill"
	}
	return api
}

// createProject makes a private project with a README on its default
// branch, named as the sweep expects (`mill-acceptance-<scenario>-<nonce>`).
func (a scratchAPI) createProject(ctx context.Context, name string) scratchProject {
	GinkgoHelper()
	var project scratchProject
	a.do(ctx, http.MethodPost, "/projects", map[string]any{
		"name":                   name,
		"path":                   name,
		"namespace_id":           a.namespaceID(ctx),
		"visibility":             "private",
		"initialize_with_readme": true,
	}, &project)
	Expect(project.DefaultBranch).NotTo(BeEmpty(), "the project has a default branch to open a merge request against")
	return project
}

// sweepByName removes the scratch project the way the worker's
// reconciliation does (internal/sweep, mill#63): listed in the namespace
// and deleted by the name the pattern matches.
func (a scratchAPI) sweepByName(ctx context.Context, name string) {
	GinkgoHelper()
	Expect(machine.IsScratch(name)).To(BeTrue(), "%s is a name the sweep matches", name)
	external := sweep.Forge(gitlab.NewScratch(a.url, a.namespace, forge.Static(a.user, a.token)))
	listed, err := external.List(ctx)
	Expect(err).NotTo(HaveOccurred())
	for _, scratch := range listed {
		if scratch.Name != name {
			continue
		}
		action, err := external.Remove(ctx, scratch)
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("swept %s: %s\n", name, action)
		return
	}
	Fail("the scratch project " + name + " was not listed in the namespace to sweep")
}

// branchHead is the commit a branch points at on the forge.
func (a scratchAPI) branchHead(ctx context.Context, project scratchProject, branch string) string {
	GinkgoHelper()
	var shown struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	a.do(ctx, http.MethodGet, "/projects/"+strconv.Itoa(project.ID)+"/repository/branches/"+url.PathEscape(branch), nil, &shown)
	return shown.Commit.ID
}

// mergeRequestBranches is the source branch of every open merge request.
func (a scratchAPI) mergeRequestBranches(ctx context.Context, project scratchProject) []string {
	GinkgoHelper()
	var listed []struct {
		SourceBranch string `json:"source_branch"`
	}
	a.do(ctx, http.MethodGet, "/projects/"+strconv.Itoa(project.ID)+"/merge_requests?state=opened", nil, &listed)
	branches := make([]string, 0, len(listed))
	for _, mr := range listed {
		branches = append(branches, mr.SourceBranch)
	}
	return branches
}

// namespaceID resolves the scratch group, by numeric id or full path.
func (a scratchAPI) namespaceID(ctx context.Context) int {
	GinkgoHelper()
	if id, err := strconv.Atoi(a.namespace); err == nil {
		return id
	}
	var namespace struct {
		ID int `json:"id"`
	}
	a.do(ctx, http.MethodGet, "/namespaces/"+url.PathEscape(a.namespace), nil, &namespace)
	return namespace.ID
}

// do makes one API call, failing on anything but a 2xx.
func (a scratchAPI) do(ctx context.Context, method, path string, body, out any) {
	GinkgoHelper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.url+"/api/v4"+path, reader)
	Expect(err).NotTo(HaveOccurred())
	request.Header.Set("PRIVATE-TOKEN", a.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	Expect(err).NotTo(HaveOccurred(), "%s %s", method, path)
	defer response.Body.Close() //nolint:errcheck // a response body the spec is done with
	payload, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.StatusCode).To(BeNumerically("<", 300), "%s %s: %s", method, path, payload)
	if out != nil {
		Expect(json.Unmarshal(payload, out)).To(Succeed(), "%s %s: %s", method, path, payload)
	}
}

// assertNoCredentialInside lists, as agent and through the workspace's own
// exec, the paths a git credential, a signing key or a forge token would
// live at, and the environment the exec was given, and finds none of them.
func assertNoCredentialInside(ctx context.Context, ws *machine.Workspace, key *signing.Key) {
	GinkgoHelper()
	process, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c",
		`id -un; for p in ~/.ssh ~/.config/gh ~/.gitconfig ~/.git-credentials ~/.config/git/config; do ` +
			`if [ -e "$p" ]; then echo "PRESENT $p"; ls -la "$p"; fi; done; echo ---ENV---; env | sort`}})
	Expect(err).NotTo(HaveOccurred())
	out, stderr, res := drainProcess(process)
	Expect(res.Status).To(BeEquivalentTo(0), stderr)
	Expect(out).To(HavePrefix("agent\n"))
	Expect(out).NotTo(ContainSubstring("PRESENT "), "no credential path exists inside:\n%s", out)
	_, env, _ := strings.Cut(out, "---ENV---\n")
	for _, line := range strings.Split(strings.TrimSpace(env), "\n") {
		name, _, _ := strings.Cut(line, "=")
		Expect(name).NotTo(BeElementOf("GIT_CONFIG_GLOBAL", "GIT_SSH_COMMAND", "MILL_FORGE_TOKEN", "GITLAB_TOKEN", "FORGE_TOKEN"), "the environment carries no credential: %s", line)
		Expect(name).NotTo(ContainSubstring("SIGNINGKEY"))
	}
	Expect(env).To(ContainSubstring("GIT_AUTHOR_NAME=" + ws.Committer().Name))
	Expect(env).To(ContainSubstring("GIT_COMMITTER_EMAIL=" + ws.Committer().Email))
	if key != nil {
		Expect(env).NotTo(ContainSubstring(key.Path), "no host path to the key reaches inside")
		Expect(out).NotTo(ContainSubstring(key.PublicKey))
	}
}
