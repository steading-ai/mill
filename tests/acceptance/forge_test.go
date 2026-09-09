package acceptance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// scratchProject is one throwaway GitLab project: created for a scenario,
// seeded with the fixture's files, and deleted when the scenario ends. It is
// the "real forge" half of the suite — nothing here is a fake, because the
// merge, the signature and the default branch are exactly what a fake would
// have to guess at.
type scratchProject struct {
	ID            int    `json:"id"`
	Path          string `json:"path_with_namespace"`
	CloneURL      string `json:"http_url_to_repo"`
	WebURL        string `json:"web_url"`
	DefaultBranch string `json:"default_branch"`
}

// forgeAPI is GitLab's v4 API, with the suite's token on every request.
type forgeAPI struct{ env Environment }

// newScratchProject creates a project in the scratch namespace, seeds it
// with files, and registers its deletion.
func newScratchProject(ctx context.Context, name string, files map[string]string) *scratchProject {
	GinkgoHelper()
	api := forgeAPI{env: env}
	slug := "mill-acceptance-" + name + "-" + nonce()

	var project scratchProject
	api.do(ctx, http.MethodPost, "/projects", map[string]any{
		"name":                   slug,
		"path":                   slug,
		"namespace_id":           api.namespaceID(ctx),
		"visibility":             "private",
		"initialize_with_readme": false,
		// The suite verifies the signature on the commit the mill made, and
		// reads it as the merge commit's second parent. Squashing would
		// replace it with one GitLab wrote, and a pipeline gate would hold
		// the merge behind CI this project does not have.
		"squash_option":                         "never",
		"merge_method":                          "merge",
		"only_allow_merge_if_pipeline_succeeds": false,
		"remove_source_branch_after_merge":      false,
	}, &project)
	DeferCleanup(func(ctx context.Context) {
		api.do(ctx, http.MethodDelete, "/projects/"+strconv.Itoa(project.ID), nil, nil)
	})

	for path, content := range files {
		api.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%d/repository/files/%s", project.ID, url.PathEscape(path)),
			map[string]any{
				"branch":         "main",
				"content":        content,
				"commit_message": "Seed " + path + " for the acceptance suite",
			}, nil)
	}
	api.do(ctx, http.MethodGet, "/projects/"+strconv.Itoa(project.ID), nil, &project)
	Expect(project.DefaultBranch).NotTo(BeEmpty(), "the seeded project has a default branch")
	return &project
}

// file is a path's content on a ref.
func (p *scratchProject) file(ctx context.Context, path, ref string) string {
	GinkgoHelper()
	api := forgeAPI{env: env}
	return api.raw(ctx, fmt.Sprintf("/projects/%d/repository/files/%s/raw?ref=%s",
		p.ID, url.PathEscape(path), url.QueryEscape(ref)))
}

// branchesContaining is every branch a commit is on, which is how the suite
// asks whether a SHA landed on the default branch.
func (p *scratchProject) branchesContaining(ctx context.Context, sha string) []string {
	GinkgoHelper()
	var refs []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	forgeAPI{env: env}.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%d/repository/commits/%s/refs?type=branch", p.ID, url.PathEscape(sha)), nil, &refs)
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

// tree is the paths under a directory on a ref, which is how the suite asks
// whether a run committed a file whose name it does not know in advance.
func (p *scratchProject) tree(ctx context.Context, path, ref string) []string {
	GinkgoHelper()
	var entries []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	forgeAPI{env: env}.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%d/repository/tree?path=%s&ref=%s", p.ID, url.QueryEscape(path), url.QueryEscape(ref)),
		nil, &entries)
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == "blob" {
			paths = append(paths, entry.Path)
		}
	}
	return paths
}

// diffOf is the files a commit changed, as GitLab reports them.
func (p *scratchProject) diffOf(ctx context.Context, sha string) []forgeDiff {
	GinkgoHelper()
	var diffs []forgeDiff
	forgeAPI{env: env}.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%d/repository/commits/%s/diff", p.ID, url.PathEscape(sha)), nil, &diffs)
	return diffs
}

// forgeDiff is one file's diff in a commit.
type forgeDiff struct {
	NewPath string `json:"new_path"`
	Diff    string `json:"diff"`
}

// mergeRequests is every merge request the project has ever had, in any
// state. A resumed or refenced run that opened a second one shows up here.
func (p *scratchProject) mergeRequests(ctx context.Context) []forgeMR {
	GinkgoHelper()
	var mrs []forgeMR
	forgeAPI{env: env}.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests?state=all&per_page=100", p.ID), nil, &mrs)
	return mrs
}

// forgeMR is a merge request as the suite reads it: who merged it, and what
// it became on the default branch.
type forgeMR struct {
	IID          int    `json:"iid"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	MergeCommit  string `json:"merge_commit_sha"`
	MergedBy     *struct {
		Username string `json:"username"`
	} `json:"merged_by"`
}

// namespaceID resolves the scratch group, given either its numeric id or its
// full path.
func (a forgeAPI) namespaceID(ctx context.Context) int {
	GinkgoHelper()
	if id, err := strconv.Atoi(a.env.ForgeNamespace); err == nil {
		return id
	}
	var namespace struct {
		ID int `json:"id"`
	}
	a.do(ctx, http.MethodGet, "/namespaces/"+url.PathEscape(a.env.ForgeNamespace), nil, &namespace)
	return namespace.ID
}

// do makes one API call, failing the spec on anything but a 2xx and decoding
// the body into out when out is not nil.
func (a forgeAPI) do(ctx context.Context, method, path string, body any, out any) {
	GinkgoHelper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.env.ForgeURL+"/api/v4"+path, reader)
	Expect(err).NotTo(HaveOccurred())
	request.Header.Set("PRIVATE-TOKEN", a.env.ForgeToken)
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

// raw fetches a path whose body is not JSON.
func (a forgeAPI) raw(ctx context.Context, path string) string {
	GinkgoHelper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.env.ForgeURL+"/api/v4"+path, nil)
	Expect(err).NotTo(HaveOccurred())
	request.Header.Set("PRIVATE-TOKEN", a.env.ForgeToken)
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	Expect(err).NotTo(HaveOccurred())
	defer response.Body.Close() //nolint:errcheck // a response body the spec is done with
	payload, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.StatusCode).To(BeNumerically("<", 300), "GET %s: %s", path, payload)
	return string(payload)
}

// nonce is what keeps two runs of the suite from colliding on a name.
func nonce() string {
	buf := make([]byte, 4)
	// crypto/rand.Read never returns an error; the signature keeps one.
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// mergeRequestFor is the one merge request on a branch, failing when there
// is none or more than one. "No duplicate MR" is an assertion this makes.
func mergeRequestFor(mrs []forgeMR, branch string) forgeMR {
	GinkgoHelper()
	var found []forgeMR
	for _, mr := range mrs {
		if mr.SourceBranch == branch {
			found = append(found, mr)
		}
	}
	Expect(found).To(HaveLen(1), "exactly one merge request on %s; got %d", branch, len(found))
	return found[0]
}
