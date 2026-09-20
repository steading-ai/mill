package cli

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
)

// The forge credential: the credential store first, the environment second,
// so a deployment can keep the token out of every process environment and a
// laptop can keep exporting it.
var _ = Describe("The forge credential", Label("unit"), func() {
	var store string

	BeforeEach(func() {
		store = GinkgoT().TempDir()
	})

	// Scenario: The forge token never touches the environment
	//   Given forge-token is in the guest's credential store and imported by the unit
	//   And MILL_FORGE_TOKEN is unset
	//   When a run delivers a commit
	//   Then the delivery authenticates with the stored token
	It("The forge token never touches the environment", func() {
		By("given forge-token is in the credential store the unit imported, and MILL_FORGE_TOKEN is unset")
		Expect(os.WriteFile(filepath.Join(store, forgeCredentialName), []byte("ghp_from_the_store\n"), 0o600)).To(Succeed()) // gitleaks:allow
		GinkgoT().Setenv("CREDENTIALS_DIRECTORY", store)
		Expect(os.Getenv(forgeTokenEnv)).To(BeEmpty())

		By("when a delivery mints its credential, the way every git command and API call does")
		credential, err := forgeMinter().Mint(GinkgoT().Context(), forge.Step{TokenID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
		Expect(err).NotTo(HaveOccurred())

		By("then it authenticates with the stored token, and the doctor names the store")
		Expect(credential.Token).To(Equal("ghp_from_the_store"))
		token, err := loadForgeToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token.source).To(ContainSubstring("credential store"))
		Expect(token.source).To(ContainSubstring("$CREDENTIALS_DIRECTORY/forge-token"))
	})

	It("falls back to $MILL_FORGE_TOKEN when the store holds no forge-token", func() {
		GinkgoT().Setenv("CREDENTIALS_DIRECTORY", store)
		GinkgoT().Setenv(forgeTokenEnv, "ghp_from_the_environment") // gitleaks:allow

		credential, err := forgeMinter().Mint(GinkgoT().Context(), forge.Step{TokenID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
		Expect(err).NotTo(HaveOccurred())
		Expect(credential.Token).To(Equal("ghp_from_the_environment"))
		token, err := loadForgeToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token.source).To(Equal("the credential in $MILL_FORGE_TOKEN"))
	})

	It("behaves as before with only MILL_FORGE_TOKEN set and no credential directory", func() {
		GinkgoT().Setenv(forgeTokenEnv, "ghp_from_the_environment") // gitleaks:allow

		credential, err := forgeMinter().Mint(GinkgoT().Context(), forge.Step{TokenID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
		Expect(err).NotTo(HaveOccurred())
		Expect(credential.Token).To(Equal("ghp_from_the_environment"))
	})

	It("prefers the store over the environment when both hold a token", func() {
		Expect(os.WriteFile(filepath.Join(store, forgeCredentialName), []byte("ghp_from_the_store"), 0o600)).To(Succeed()) // gitleaks:allow
		GinkgoT().Setenv("CREDENTIALS_DIRECTORY", store)
		GinkgoT().Setenv(forgeTokenEnv, "ghp_from_the_environment") // gitleaks:allow

		credential, err := forgeMinter().Mint(GinkgoT().Context(), forge.Step{TokenID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
		Expect(err).NotTo(HaveOccurred())
		Expect(credential.Token).To(Equal("ghp_from_the_store"))
	})

	It("refuses a delivered file that is empty rather than falling through", func() {
		Expect(os.WriteFile(filepath.Join(store, forgeCredentialName), []byte("\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("CREDENTIALS_DIRECTORY", store)
		GinkgoT().Setenv(forgeTokenEnv, "ghp_from_the_environment") // gitleaks:allow

		_, err := forgeMinter().Mint(GinkgoT().Context(), forge.Step{TokenID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
		Expect(err).To(MatchError(ContainSubstring("is empty")))
	})

	It("holds no credential when neither source has one", func() {
		_, err := forgeMinter().Mint(GinkgoT().Context(), forge.Step{TokenID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
		Expect(err).To(MatchError(ContainSubstring("no credential")))
	})
})
