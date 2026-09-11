package machine_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine"
)

var _ = Describe("The systemd driver's image verbs", Label("unit"), func() {
	var (
		h      *host
		driver machine.Driver
	)

	BeforeEach(func() {
		h = newHost()
		driver = machine.NewSystemd(h.spawner, h.commands)
	})

	It("pulls and reads a digest through podman", func(ctx context.Context) {
		h.stub("podman", `case "$1" in inspect|image) echo "sha256:abc"; esac; exit 0`)

		Expect(driver.PullImage(ctx, "harbor.example/mill/base:latest")).To(Succeed())
		digest, err := driver.ImageDigest(ctx, "harbor.example/mill/base:latest")
		Expect(err).NotTo(HaveOccurred())
		Expect(digest).To(Equal("sha256:abc"))
		Expect(h.calls("podman")).To(Equal([]string{
			"pull", "--quiet", "harbor.example/mill/base:latest",
			"image", "inspect", "--format", "{{.Digest}}", "harbor.example/mill/base:latest",
		}))
	})

	It("builds on the host with every tag and build argument, returning the transcript whole", func(ctx context.Context) {
		h.stub("podman", `echo "STEP 1/2: FROM base"; echo "No match for argument: libfooo-devel" >&2; echo "Error: building at STEP" >&2; exit 1`)

		output, err := driver.BuildImageOnHost(ctx, machine.ImageBuild{
			Context:       "/tmp/ctx",
			Containerfile: "/tmp/ctx/Containerfile",
			Tags:          []string{"localhost/a:1", "localhost/a:latest"},
			Args:          map[string]string{"HARBOR_HOST": "harbor.example"},
		})
		Expect(err).To(MatchError(ContainSubstring("exited 1")))
		Expect(output).To(ContainSubstring("STEP 1/2: FROM base"))
		Expect(output).To(ContainSubstring("No match for argument: libfooo-devel"))
		Expect(h.calls("podman")).To(Equal([]string{
			"build", "--tag", "localhost/a:1", "--tag", "localhost/a:latest",
			"--file", "/tmp/ctx/Containerfile", "--build-arg", "HARBOR_HOST=harbor.example", "/tmp/ctx",
		}))

		_, err = driver.BuildImageOnHost(ctx, machine.ImageBuild{Context: "/tmp/ctx"})
		Expect(err).To(MatchError(ContainSubstring("no tag")))
	})

	It("exports by piping podman export into tar in the subvolume, and removes the container", func(ctx context.Context) {
		h.stub("podman", `case "$1" in create) echo c0ffee;; export) printf 'tarball';; esac; exit 0`)
		h.stub("tar", `cat > "$(dirname "$0")/tar.stdin"; exit 0`)

		Expect(driver.ExportImage(ctx, "localhost/mill/derived:d", "/var/lib/machines/mill-image-d.building")).To(Succeed())

		Expect(h.calls("podman")).To(Equal([]string{
			"create", "localhost/mill/derived:d",
			"export", "c0ffee",
			"rm", "--force", "c0ffee",
		}))
		Expect(h.calls("tar")).To(Equal([]string{"--extract", "--directory", "/var/lib/machines/mill-image-d.building", "--file", "-"}))
		stdin, err := os.ReadFile(filepath.Join(h.dir, "tar.stdin"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(stdin)).To(Equal("tarball"))
	})

	It("reports an export whose extraction failed, and still removes the container", func(ctx context.Context) {
		h.stub("podman", `case "$1" in create) echo c0ffee;; export) printf 'tarball';; esac; exit 0`)
		h.stub("tar", `echo "tar: no space left" >&2; exit 2`)

		err := driver.ExportImage(ctx, "localhost/mill/derived:d", "/x")
		Expect(err).To(MatchError(ContainSubstring("tar exited 2")))
		Expect(err).To(MatchError(ContainSubstring("no space left")))
		Expect(h.calls("podman")).To(ContainElements("rm", "--force", "c0ffee"))
	})

	It("creates, renames and deletes subvolumes and sets capabilities as the worker when no sudo is named", func(ctx context.Context) {
		h.stub("btrfs", "exit 0")
		h.stub("setcap", "exit 0")
		dir := GinkgoT().TempDir()
		Expect(os.Mkdir(filepath.Join(dir, "a.building"), 0o755)).To(Succeed())

		Expect(driver.CreateSubvolume(ctx, "/x/a.building")).To(Succeed())
		Expect(driver.Setcap(ctx, "cap_setuid=ep", "/x/a.building/usr/bin/newuidmap")).To(Succeed())
		Expect(driver.Rename(ctx, filepath.Join(dir, "a.building"), filepath.Join(dir, "a"))).To(Succeed())
		Expect(filepath.Join(dir, "a")).To(BeADirectory())
		present, err := driver.Exists(ctx, filepath.Join(dir, "a"))
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeTrue())
		present, err = driver.Exists(ctx, filepath.Join(dir, "absent"))
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeFalse())
		Expect(driver.DeleteSubvolume(ctx, "/x/a")).To(Succeed())

		Expect(h.calls("btrfs")).To(Equal([]string{"subvolume", "create", "/x/a.building", "subvolume", "delete", "/x/a"}))
		Expect(h.calls("setcap")).To(Equal([]string{"cap_setuid=ep", "/x/a.building/usr/bin/newuidmap"}))
		Expect(h.floor("btrfs")).To(Equal("through-the-spawner"))
	})

	It("writes the image directory through sudo when it is named, and podman never", func(ctx context.Context) {
		h.stub("sudo", `exit 0`)
		h.stub("podman", `exit 0`)
		commands := h.commands
		commands.Sudo = filepath.Join(h.dir, "sudo")
		privileged := machine.NewSystemd(h.spawner, commands)

		Expect(privileged.CreateSubvolume(ctx, "/x/a.building")).To(Succeed())
		Expect(privileged.Setcap(ctx, "cap_setgid=ep", "/x/a.building/usr/bin/newgidmap")).To(Succeed())
		Expect(privileged.Rename(ctx, "/x/a.building", "/x/a")).To(Succeed())
		present, err := privileged.Exists(ctx, "/x/a")
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeTrue())
		Expect(privileged.DeleteSubvolume(ctx, "/x/a")).To(Succeed())
		Expect(privileged.RemoveImage(ctx, "localhost/mill/derived:d")).To(Succeed())

		Expect(h.calls("sudo")).To(Equal([]string{
			"-n", h.commands.Btrfs, "subvolume", "create", "/x/a.building",
			"-n", h.commands.Setcap, "cap_setgid=ep", "/x/a.building/usr/bin/newgidmap",
			"-n", "mv", "-T", "/x/a.building", "/x/a",
			"-n", "test", "-e", "/x/a",
			"-n", h.commands.Btrfs, "subvolume", "delete", "/x/a",
		}))
		Expect(h.calls("podman")).To(Equal([]string{"rmi", "localhost/mill/derived:d"}))
		Expect(h.calls("btrfs")).To(BeEmpty(), "btrfs was reached through sudo and not directly")
	})

	It("logs in with the secret on stdin and never on the command line, pushes, and logs out", func(ctx context.Context) {
		h.stub("podman", `if [ "$1" = login ]; then cat > "$(dirname "$0")/podman.stdin"; fi; exit 0`)

		Expect(driver.Login(ctx, "harbor.example", "robot$mill+builder", "s3cret")).To(Succeed())
		Expect(driver.PushImage(ctx, "harbor.example/mill/base:latest")).To(Succeed())
		Expect(driver.Logout(ctx, "harbor.example")).To(Succeed())

		Expect(h.calls("podman")).To(Equal([]string{
			"login", "--username", "robot$mill+builder", "--password-stdin", "harbor.example",
			"push", "harbor.example/mill/base:latest",
			"logout", "harbor.example",
		}))
		Expect(h.calls("podman")).NotTo(ContainElement("s3cret"))
		stdin, err := os.ReadFile(filepath.Join(h.dir, "podman.stdin"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(stdin)).To(Equal("s3cret"))
	})

	It("kills a command that outlives its context, which is how a build budget is enforced", func(ctx context.Context) {
		h.stub("podman", `sleep 30; exit 0`)
		budget, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()

		started := time.Now()
		_, err := driver.BuildImageOnHost(budget, machine.ImageBuild{Context: "/tmp/ctx", Tags: []string{"t"}})
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second))
	})
})
