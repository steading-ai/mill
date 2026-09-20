package doctor_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/doctor"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/signing"
)

// fakeContentReader is a project's repository, for the specs about the
// signing probe's fallback: it answers the two names an allowed_signers
// file might have, or the failure a real forge would answer with.
type fakeContentReader struct {
	files map[string]string
	err   error
}

func (f *fakeContentReader) ReadFile(_ context.Context, path string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if content, ok := f.files[path]; ok {
		return content, nil
	}
	return "", forge.ErrNoSuchFile
}

var _ = Describe("The signing probe", Label("unit"), func() {
	var workspaceDir string

	generate := func(ctx context.Context) *signing.Key {
		GinkgoHelper()
		key, err := signing.Generate(ctx, workspace.Spawner{Floor: []string{"PATH=" + os.Getenv("PATH")}},
			filepath.Join(workspaceDir, "signing"), "mill", "mill@example.test")
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	BeforeEach(func() { workspaceDir = GinkgoT().TempDir() })

	It("passes on a host with a key and no checkout when the project's repository trusts it", func(ctx context.Context) {
		key := generate(ctx)
		source := &fakeContentReader{files: map[string]string{"allowed_signers": key.AllowedSignersLine() + "\n"}}

		result := doctor.Signing(ctx, workspaceDir, "mill", "mill@example.test", source)
		Expect(result.OK).To(BeTrue(), result.Reason)
		Expect(result.Reason).To(ContainSubstring("trusted by the project's repository"))
	})

	It("fails when the project's repository has no such file", func(ctx context.Context) {
		generate(ctx)
		source := &fakeContentReader{files: map[string]string{}}

		result := doctor.Signing(ctx, workspaceDir, "mill", "mill@example.test", source)
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("no project's allowed_signers names it"))
	})

	It("fails the same way when the forge has no way to read a file at all", func(ctx context.Context) {
		generate(ctx)

		result := doctor.Signing(ctx, workspaceDir, "mill", "mill@example.test", nil)
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("no project's allowed_signers names it"))
	})

	It("surfaces a repository read failure rather than reporting it as untrusted", func(ctx context.Context) {
		generate(ctx)
		source := &fakeContentReader{err: forge.ErrNoPullRequest}

		result := doctor.Signing(ctx, workspaceDir, "mill", "mill@example.test", source)
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).NotTo(ContainSubstring("no project's allowed_signers names it"))
	})
})
