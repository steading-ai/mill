package nspawn_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The cache lineage of docs/sandbox.md §Caching and the derived image of
// §Images, as the write-up's E9 and E10 ran them. The lineage specs share
// one fill — a multi-gigabyte Go build cache takes a minute to make and
// seconds to snapshot, which is the point — and every subvolume they make
// is the operator's, under /var/lib/mill, created and deleted without sudo.
// The image specs drive build-derived.sh; its subvolumes are root's under
// /var/lib/machines.
const (
	lineageRoot   = "/var/lib/mill"
	lineageTarget = lineageRoot + "/cache-espec-target"
	lineageGen1   = lineageRoot + "/cache-espec-gen-1"
	lineageGen2   = lineageRoot + "/cache-espec-gen-2"
	lineageRun    = lineageRoot + "/run-espec-target"
	cacheMachine  = "mill-espec-cache"
	derivedScript = "build-derived.sh"
	baseImage     = "mill-base-spike"
	derivedPkg    = "libpq-devel"
	missingPkg    = "no-such-package-espec"
	machinesDir   = "/var/lib/machines"
	// A generation counts as multi-gigabyte at two GiB of apparent size,
	// which is what du -s reports and §Promote records.
	multiGigabyte = 2 * 1024 * 1024
)

// btrfs runs a btrfs subcommand as the operator and fails the spec if it
// asked for a privilege it must not need.
func btrfs(args ...string) string {
	GinkgoHelper()
	out, stderr, status := run(nil, append([]string{"btrfs"}, args...)...)
	Expect(status).To(BeZero(), "btrfs %s as uid %d: %s", strings.Join(args, " "), os.Getuid(), stderr)
	return out
}

// deleteLineage removes a subvolume its owner made, clearing the read-only
// flag a promoted generation carries first, and forces the commit that
// returns its space.
func deleteLineage(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	run(nil, "btrfs", "property", "set", "-ts", path, "ro", "false")
	run(nil, "btrfs", "subvolume", "delete", path)
	run(nil, "btrfs", "filesystem", "sync", lineageRoot)
}

// fill makes the target subvolume a Go build cache of this repository large
// enough to count as multi-gigabyte: the build and the test binaries, then
// the same with the race detector, for two other architectures and for a
// second amd64 level, each of which is a distinct set of cache entries.
func fill(repo, goBin string) {
	GinkgoHelper()
	env := append(os.Environ(),
		"GOCACHE="+lineageTarget,
		"GOMODCACHE="+lineageTarget+"/mod",
		"GOTOOLCHAIN=local",
	)
	build := func(extraEnv []string, args ...string) {
		cmd := exec.Command(goBin, args...)
		cmd.Dir = repo
		cmd.Env = append(append([]string{}, env...), extraEnv...)
		cmd.Stdout, cmd.Stderr = GinkgoWriter, GinkgoWriter
		Expect(cmd.Run()).To(Succeed(), "go %s", strings.Join(args, " "))
	}
	build(nil, "build", "./...")
	build(nil, "test", "-count=1", "-run", "xxx", "./...")
	build(nil, "build", "-race", "./...")
	build(nil, "test", "-race", "-count=1", "-run", "xxx", "./...")
	for _, variant := range []string{"GOARCH=arm64", "GOARCH=riscv64", "GOAMD64=v3"} {
		build([]string{variant}, "build", "./...")
		build([]string{variant}, "test", "-count=1", "-run", "xxx", "-exec", "/bin/true", "./...")
	}
}

// apparentKiB is du -s of a tree: the size estimate §Promote records.
func apparentKiB(path string) int {
	GinkgoHelper()
	out, stderr, status := run(nil, "du", "-s", path)
	Expect(status).To(BeZero(), stderr)
	kib, err := strconv.Atoi(strings.Fields(out)[0])
	Expect(err).NotTo(HaveOccurred())
	return kib
}

// timed runs a btrfs subcommand and returns how long it took.
func timed(args ...string) time.Duration {
	GinkgoHelper()
	start := time.Now()
	btrfs(args...)
	return time.Since(start)
}

// files lists the regular files under root, relative to it.
func files(root string) map[string]bool {
	GinkgoHelper()
	seen := map[string]bool{}
	Expect(filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			seen[rel] = true
		}
		return nil
	})).To(Succeed())
	return seen
}

func sha256Of(path string) string {
	GinkgoHelper()
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// derivedDigest is the script's name for a (base, packages) pair.
func derivedDigest(base string, packages ...string) string {
	GinkgoHelper()
	out, _, status := run(nil, "sh", "-c", "sha256sum <<<\""+base+" "+strings.Join(packages, " ")+"\" | cut -c1-16")
	Expect(status).To(BeZero())
	return strings.TrimSpace(out)
}

// buildDerived runs build-derived.sh for the package list and returns what
// it said and how it ended; the subvolume it may have made is removed with
// the spec.
func buildDerived(packages ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	script, err := filepath.Abs(derivedScript)
	Expect(err).NotTo(HaveOccurred())
	return run(nil, append([]string{script, baseImage}, packages...)...)
}

func cleanupDerived(digest string) {
	subvolume := machinesDir + "/mill-image-" + digest
	run(nil, "sudo", "sh", "-c", "test -d "+subvolume+" && btrfs subvolume delete "+subvolume+"; rm -f "+lineageRoot+"/image-"+digest+".lock; true")
	run(nil, "podman", "rmi", "mill-derived-"+digest)
}

var _ = Describe("Cache lineages and derived images on a real machine", Label("live"), Serial, func() {
	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine")
		}
		if _, _, status := run(nil, "sudo", "test", "-d", image); status != 0 {
			Skip(image + " is missing; spikes/nspawn/build-image.sh builds it")
		}
	})

	Describe("a lineage", Ordered, func() {
		var repo, goBin, goRoot string

		BeforeAll(func() {
			if os.Getenv(liveVar) == "" {
				Skip("set " + liveVar + "=1 to run the specs that fill and snapshot a lineage")
			}
			var err error
			repo, err = filepath.Abs("../..")
			Expect(err).NotTo(HaveOccurred())
			goBin, err = exec.LookPath("go")
			Expect(err).NotTo(HaveOccurred(), "the fill needs go on the host")
			out, _, status := run(nil, goBin, "env", "GOROOT")
			Expect(status).To(BeZero())
			goRoot = strings.TrimSpace(out)

			for _, s := range []string{lineageRun, lineageGen2, lineageGen1, lineageTarget} {
				deleteLineage(s)
			}
			DeferCleanup(func() {
				for _, s := range []string{lineageRun, lineageGen2, lineageGen1, lineageTarget} {
					deleteLineage(s)
				}
			})

			By("a lineage generation holding a multi-gigabyte build cache")
			btrfs("subvolume", "create", lineageTarget)
			fill(repo, goBin)
			Expect(apparentKiB(lineageTarget)).To(BeNumerically(">=", multiGigabyte), "the fill did not reach two gigabytes")
			btrfs("subvolume", "snapshot", "-r", lineageTarget, lineageGen1)
		})

		It("Restore and promote are constant time", func() {
			By("a run restores it by snapshot")
			restore := timed("subvolume", "snapshot", lineageGen1, lineageRun)

			By("and promotes its copy by read-only snapshot")
			promote := timed("subvolume", "snapshot", "-r", lineageRun, lineageGen2)

			By("each snapshot completes in under a second")
			size := apparentKiB(lineageGen1)
			GinkgoWriter.Printf("restore %s, promote %s, generation %d KiB\n", restore, promote, size)
			Expect(restore).To(BeNumerically("<", time.Second))
			Expect(promote).To(BeNumerically("<", time.Second))

			// The next scenario promotes the run's own writes; this one only
			// measured the promote.
			deleteLineage(lineageGen2)
		})

		It("Generations are independent", func() {
			By("a run working on a snapshot of gen-1")
			if _, err := os.Stat(lineageRun); err != nil {
				btrfs("subvolume", "snapshot", lineageGen1, lineageRun)
			}
			mount := filepath.Join(repo, ".cache", "go")
			Expect(os.MkdirAll(mount, 0o755)).To(Succeed())
			DeferCleanup(func() {
				_ = os.Remove(mount)
				_ = os.Remove(filepath.Dir(mount))
			})
			boot(cacheMachine,
				"--bind="+repo+":/work:idmap",
				"--bind="+lineageRun+":/work/.cache/go:idmap",
				"--bind-ro="+goRoot+":/opt/go:idmap")
			goEnv := []string{"User=agent", "WorkingDirectory=/work", "Environment=HOME=/home/agent",
				"Environment=GOCACHE=/work/.cache/go", "Environment=GOMODCACHE=/work/.cache/go/mod",
				"Environment=GOTOOLCHAIN=local", "Environment=GOPROXY=off", "Environment=GOFLAGS=-mod=mod"}
			_, stderr, status := execIn(cacheMachine, goEnv, "/opt/go/bin/go", "build", "-tags", "espec", "-o", "/tmp/mill-espec", "./cmd/mill")
			Expect(status).To(BeZero(), stderr)
			before := files(lineageGen1)
			btrfs("subvolume", "snapshot", "-r", lineageRun, lineageGen2)

			var written string
			for f := range files(lineageGen2) {
				if !before[f] && strings.HasSuffix(f, "-d") {
					written = f
					break
				}
			}
			Expect(written).NotTo(BeEmpty(), "the run's build wrote nothing new into its copy")
			sum := sha256Of(filepath.Join(lineageGen2, written))

			By("gen-1 is deleted and synced")
			btrfs("property", "set", "-ts", lineageGen1, "ro", "false")
			btrfs("subvolume", "delete", lineageGen1)
			btrfs("filesystem", "sync", lineageRoot)
			Expect(lineageGen1).NotTo(BeAnExistingFile())

			By("the run's files are intact")
			inside, stderr, status := execIn(cacheMachine, []string{"User=agent"}, "sha256sum", "/work/.cache/go/"+written)
			Expect(status).To(BeZero(), stderr)
			Expect(strings.Fields(inside)[0]).To(Equal(sum))
			_, stderr, status = execIn(cacheMachine, goEnv, "sh", "-c",
				"/opt/go/bin/go build -tags espec -o /tmp/mill-espec-again ./cmd/mill && cmp /tmp/mill-espec /tmp/mill-espec-again")
			Expect(status).To(BeZero(), stderr)

			By("and gen-2 holds the run's writes")
			Expect(filepath.Join(lineageGen2, written)).To(BeAnExistingFile())
			Expect(sha256Of(filepath.Join(lineageRun, written))).To(Equal(sum))
		})
	})

	Describe("a derived image", func() {
		BeforeEach(func() {
			if _, _, status := run(nil, "podman", "image", "exists", baseImage); status != 0 {
				Skip(baseImage + " is not in the host's podman store; spikes/nspawn/build-image.sh builds it")
			}
		})

		It("A derived image builds once under the lock", func() {
			digest := derivedDigest(baseImage, derivedPkg)
			cleanupDerived(digest)
			DeferCleanup(func() { cleanupDerived(digest) })

			By("two builds of the same digest started together")
			type result struct {
				stdout, stderr string
				status         int
			}
			results := make([]result, 2)
			var wg sync.WaitGroup
			for i := range results {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					defer GinkgoRecover()
					stdout, stderr, status := buildDerived(derivedPkg)
					results[i] = result{stdout, stderr, status}
				}(i)
				time.Sleep(time.Second)
			}
			wg.Wait()

			By("one builds, the other waits and finds the image")
			var built, present int
			for _, r := range results {
				Expect(r.status).To(BeZero(), r.stderr)
				switch {
				case strings.HasPrefix(r.stdout, "built:"):
					built++
				case strings.HasPrefix(r.stdout, "present:"):
					present++
				}
				GinkgoWriter.Print(r.stdout)
			}
			Expect(built).To(Equal(1))
			Expect(present).To(Equal(1))

			By("and the machine booted from it has the package")
			subvolume := machinesDir + "/mill-image-" + digest
			machine := "mill-espec-derived"
			unit := "mill-run-" + machine
			flags := strings.ReplaceAll(bootFlags, "--directory="+image, "--directory="+subvolume)
			argv := append([]string{"sudo", "systemd-run", "--unit=" + unit, "--collect", "-p", "KillMode=mixed",
				"systemd-nspawn", "--machine=" + machine}, strings.Fields(flags)...)
			_, stderr, status := run(nil, argv...)
			Expect(status).To(BeZero(), stderr)
			DeferCleanup(func() { run(nil, "sudo", "systemctl", "stop", unit) })
			Eventually(func() int {
				_, _, status := execIn(machine, nil, "true")
				return status
			}).WithTimeout(30*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(), "the machine's bus never answered")
			out, stderr, status := execIn(machine, []string{"User=agent"}, "rpm", "-q", derivedPkg)
			Expect(status).To(BeZero(), stderr)
			Expect(out).To(HavePrefix(derivedPkg + "-"))
		})

		It("A failed derived build leaves nothing", func() {
			By("a package that does not exist")
			digest := derivedDigest(baseImage, missingPkg)
			DeferCleanup(func() { cleanupDerived(digest) })

			By("the derived build runs")
			stdout, stderr, status := buildDerived(missingPkg)

			By("it fails")
			Expect(status).To(Equal(1), stdout)

			By("no .building subvolume remains")
			listing, _, _ := run(nil, "sudo", "ls", machinesDir)
			Expect(listing).NotTo(ContainSubstring(".building"))
			Expect(listing).NotTo(ContainSubstring("mill-image-" + digest))

			By("and dnf's last lines are captured")
			Expect(stderr).To(ContainSubstring("No match for argument: " + missingPkg))
		})
	})
})
