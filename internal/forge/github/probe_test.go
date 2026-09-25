package github_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge/github"
)

var _ = Describe("The GitHub credential probe", Label("unit"), func() {
	var (
		api    *stub
		client *github.Client
	)

	const mainHead = "1111111111111111111111111111111111111111"

	BeforeEach(func() {
		api = newStub()
		client = github.New(api.repo(), api.minter(), github.WithHTTPClient(api.httpClient()))
	})

	// everything routes the reads a delivery makes, so a spec can then
	// take one away.
	everything := func() {
		api.on(http.MethodGet, "/user", http.StatusOK, "user.json")
		api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
		api.on(http.MethodGet, repoAPI+"/branches/main", http.StatusOK, "branch_main.json")
		api.on(http.MethodGet, repoAPI+"/commits/"+mainHead+"/check-runs", http.StatusOK, "check_runs_empty.json")
		api.on(http.MethodGet, repoAPI+"/commits/"+mainHead+"/status", http.StatusOK, "status_empty.json")
		api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_empty.json")
	}

	It("names the account the token authenticates as, having read what a delivery reads", func(ctx context.Context) {
		everything()

		account, err := client.Probe(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(account.Login).To(Equal("octocat"))

		var paths []string
		for _, seen := range api.calls() {
			paths = append(paths, seen.Path)
		}
		Expect(paths).To(ConsistOf(
			"/user", repoAPI, repoAPI+"/branches/main",
			repoAPI+"/commits/"+mainHead+"/check-runs", repoAPI+"/commits/"+mainHead+"/status",
			repoAPI+"/pulls",
		), "one read per permission the token needs")
	})

	It("An expired token is diagnosed before a run wastes work", func(ctx context.Context) {
		api.on(http.MethodGet, "/user", http.StatusUnauthorized, "bad_credentials.json")

		_, err := client.Probe(ctx)

		var expired *github.CredentialError
		Expect(errors.As(err, &expired)).To(BeTrue(), "%v", err)
		Expect(err.Error()).To(ContainSubstring("expired or been revoked"))
		Expect(err.Error()).To(ContainSubstring("gh auth token"), "the remedy most people already have")
		Expect(api.count(http.MethodGet, repoAPI)).To(BeZero(), "nothing else was asked of a token GitHub will not honour")
	})

	It("names the permission a fine-grained token lacks, as GitHub names it on the refusal", func(ctx context.Context) {
		everything()
		api.onWith(http.MethodGet, repoAPI+"/pulls", http.StatusForbidden, "not_accessible.json",
			map[string]string{"X-Accepted-GitHub-Permissions": "pull_requests=read"})

		_, err := client.Probe(ctx)

		var lacking *github.PermissionError
		Expect(errors.As(err, &lacking)).To(BeTrue(), "%v", err)
		Expect(lacking.Permission).NotTo(BeNil())
		Expect(lacking.Permission.Name).To(Equal("Pull requests"))
		Expect(err.Error()).To(ContainSubstring("lacks Pull requests: read and write on octo-org/thing"))
	})

	It("names the permission by the endpoint when GitHub refuses without saying which", func(ctx context.Context) {
		everything()
		api.on(http.MethodGet, repoAPI+"/commits/"+mainHead+"/check-runs", http.StatusForbidden, "not_accessible.json")

		_, err := client.Probe(ctx)

		var lacking *github.PermissionError
		Expect(errors.As(err, &lacking)).To(BeTrue(), "%v", err)
		Expect(lacking.Permission.Name).To(Equal("Checks"))
	})

	It("tells a token that cannot see the repository from one that cannot read it", func(ctx context.Context) {
		api.on(http.MethodGet, "/user", http.StatusOK, "user.json")
		api.on(http.MethodGet, repoAPI, http.StatusNotFound, "not_found.json")

		_, err := client.Probe(ctx)

		var lacking *github.PermissionError
		Expect(errors.As(err, &lacking)).To(BeTrue(), "%v", err)
		Expect(lacking.Permission).To(BeNil())
		Expect(err.Error()).To(ContainSubstring("cannot reach octo-org/thing"))
	})

	// The permission list is written down in docs/tools.md, and it has to
	// be the list the probe reads against: a permission added to one and
	// not the other is a token that passes doctor and fails at delivery,
	// or a document that asks for more than a delivery needs.
	It("is the permission list docs/tools.md carries, to the word", func() {
		body, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "tools.md"))
		Expect(err).NotTo(HaveOccurred())
		doc := string(body)
		for _, permission := range github.Permissions {
			row := "| " + permission.Name + " | " + permission.Access + " | " + permission.Why + " |"
			Expect(doc).To(ContainSubstring(row), "docs/tools.md does not carry the row:\n%s", row)
		}
		// And nothing more: every row of that table names a permission
		// the adapter knows.
		start := strings.Index(doc, "| Permission | Access | Used for |")
		Expect(start).To(BeNumerically(">=", 0), "docs/tools.md has no permission table")
		table := doc[start:]
		if end := strings.Index(table, "\n\n"); end >= 0 {
			table = table[:end]
		}
		rows := strings.Split(strings.TrimSpace(table), "\n")[2:]
		Expect(rows).To(HaveLen(len(github.Permissions)), "the table carries a row the adapter does not know:\n%s", table)
	})
})
