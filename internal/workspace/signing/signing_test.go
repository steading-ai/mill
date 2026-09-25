package signing_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/signing"
)

var _ = Describe("The mill's signing key", Label("unit"), func() {
	var dir string

	generate := func(ctx context.Context) *signing.Key {
		GinkgoHelper()
		key, err := signing.Generate(ctx, workspace.Spawner{Floor: []string{"PATH=" + os.Getenv("PATH")}}, dir, "mill", "mill@example.test")
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	BeforeEach(func() { dir = filepath.Join(GinkgoT().TempDir(), "signing") })

	It("generates a key and the allowed_signers file that verifies it", func(ctx context.Context) {
		key := generate(ctx)

		Expect(key.Path).To(BeAnExistingFile())
		Expect(key.PublicKey).To(HavePrefix("ssh-ed25519 "))
		Expect(os.ReadFile(key.AllowedSigners)).To(BeEquivalentTo(key.AllowedSignersLine() + "\n"))
	})

	It("trusts the key for git signatures made by the mill's identity, and for nothing else", func(ctx context.Context) {
		key := generate(ctx)

		Expect(key.AllowedSignersLine()).To(HavePrefix(`mill@example.test namespaces="git" ssh-ed25519 `))
		// The comment on a public key line is free text and would be read as
		// another allowed_signers field.
		Expect(key.AllowedSignersLine()).NotTo(ContainSubstring("mill@example.test ssh-ed25519"))
		Expect(key.AllowedSignersLine()).To(HaveSuffix(splitKey(key.PublicKey)))
	})

	It("configures git to sign with the key and verify against that file", func(ctx context.Context) {
		key := generate(ctx)

		Expect(key.GitConfig()).To(Equal([]string{
			"-c", "gpg.format=ssh",
			"-c", "user.signingkey=" + key.Path,
			"-c", "gpg.ssh.allowedSignersFile=" + key.AllowedSigners,
			"-c", "commit.gpgsign=true",
			"-c", "user.name=mill",
			"-c", "user.email=mill@example.test",
		}))
	})

	It("reuses a key that is already in the directory", func(ctx context.Context) {
		first := generate(ctx)

		second := generate(ctx)

		Expect(second.PublicKey).To(Equal(first.PublicKey))
	})

	It("refuses to generate a key with no identity to name in allowed_signers", func(ctx context.Context) {
		_, err := signing.Generate(ctx, workspace.Spawner{}, dir, "mill", "")

		Expect(err).To(MatchError(ContainSubstring("no identity")))
	})
})

// splitKey is the base64 half of an authorized_keys line.
func splitKey(public string) string {
	return strings.Fields(public)[1]
}
