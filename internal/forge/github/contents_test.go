package github_test

import (
	"context"
	"errors"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/forge/github"
)

var _ = Describe("The GitHub content reader", Label("unit"), func() {
	var (
		api    *stub
		client *github.Client
	)

	BeforeEach(func() {
		api = newStub()
		client = github.New(api.repo(), api.minter(), github.WithHTTPClient(api.httpClient()))
	})

	It("reads a file off the repository's default branch, decoded", func(ctx context.Context) {
		api.on(http.MethodGet, repoAPI+"/contents/allowed_signers", http.StatusOK, "allowed_signers.json")

		content, err := client.ReadFile(ctx, "allowed_signers")
		Expect(err).NotTo(HaveOccurred())
		Expect(content).To(Equal("user@example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIQfakekeymaterial\n"))
	})

	It("names a repository with no such file, rather than the error a decode would give", func(ctx context.Context) {
		api.on(http.MethodGet, repoAPI+"/contents/allowed_signers", http.StatusNotFound, "not_found.json")

		_, err := client.ReadFile(ctx, "allowed_signers")
		Expect(errors.Is(err, forge.ErrNoSuchFile)).To(BeTrue(), "%v", err)
	})

	It("diagnoses a token GitHub will not honour, the way the credential probe does", func(ctx context.Context) {
		api.on(http.MethodGet, repoAPI+"/contents/allowed_signers", http.StatusUnauthorized, "bad_credentials.json")

		_, err := client.ReadFile(ctx, "allowed_signers")

		var expired *github.CredentialError
		Expect(errors.As(err, &expired)).To(BeTrue(), "%v", err)
	})
})
