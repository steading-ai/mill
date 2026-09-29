package local_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/local"
)

// capture runs path through execer and returns its trimmed stdout, failing
// the spec if it exits non-zero.
func capture(execer workspace.Execer, command workspace.Command) string {
	GinkgoHelper()
	process, err := execer.Exec(context.Background(), command)
	Expect(err).NotTo(HaveOccurred())
	_ = process.Stdin.Close()
	out, err := io.ReadAll(process.Stdout)
	Expect(err).NotTo(HaveOccurred())
	errOut, err := io.ReadAll(process.Stderr)
	Expect(err).NotTo(HaveOccurred())
	result := process.Wait()
	Expect(result.Status).To(BeZero(), "stderr: %s", errOut)
	return strings.TrimSpace(string(out))
}

var _ = Describe("The local workspace's step boundary", Label("unit"), func() {
	var (
		root string
		from string
		ws   *local.Workspace
	)

	BeforeEach(func(ctx context.Context) {
		root = filepath.Join(GinkgoT().TempDir(), "workspaces")
		from = origin()
		ws = local.New(local.Options{
			Root:     root,
			Origin:   from,
			Identity: "mill@example.test",
			Name:     "mill",
			// Short enough that a group-mode EndStep's SIGTERM-grace-SIGKILL
			// does not make every spec here wait out the 5s default.
			Grace: 50 * time.Millisecond,
		})
		_, err := ws.Create(ctx, workspace.Spec{RunID: "run-01", NodeRef: "mill#12", Origin: from})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
	})

	It("gives a step's commands a scratch TMPDIR, reached the same way through Exec and SessionExec, gone when the step ends", func(ctx context.Context) {
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		tmp := capture(ws, workspace.Command{Path: "/bin/sh", Args: []string{"-c", `printf %s "$TMPDIR"`}})
		Expect(tmp).To(BeADirectory())
		Expect(tmp).To(HavePrefix(root))

		// SessionExec is what the harness adapter's session sidecar runs
		// through: it calls with ClearEnv true and an environment it has
		// already built itself, so the boundary's TMPDIR must be added to
		// that environment, not replace it (docs/plans/2026-09-29-mill-96.md's
		// Risks).
		out := capture(ws.SessionExec(), workspace.Command{
			Path: "/bin/sh", Args: []string{"-c", `printf '%s|%s' "$TMPDIR" "$CALLER_OWN_VAR"`},
			Env:      map[string]string{"CALLER_OWN_VAR": "kept"},
			ClearEnv: true,
		})
		Expect(out).To(Equal(tmp + "|kept"))

		Expect(ws.EndStep(ctx, 1, true)).To(Succeed())
		Expect(tmp).NotTo(BeADirectory())
	})

	It("treats BeginStep at an ordinal already open as a crashed attempt's leftovers, not a conflict", func(ctx context.Context) {
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		tmp := capture(ws, workspace.Command{Path: "/bin/sh", Args: []string{"-c", `printf %s "$TMPDIR"`}})
		marker := filepath.Join(tmp, "leftover")
		Expect(os.WriteFile(marker, []byte("x"), 0o600)).To(Succeed())

		// No EndStep in between: a worker that crashed mid-step and resumes
		// starts its ordinal count over at 1 (local does not implement
		// workspace.StepResumer), which can collide with the crashed
		// attempt's own leftovers under the same name.
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		again := capture(ws, workspace.Command{Path: "/bin/sh", Args: []string{"-c", `printf %s "$TMPDIR"`}})
		Expect(again).To(Equal(tmp))
		Expect(marker).NotTo(BeAnExistingFile())
	})

	It("ends every process a step's session started, including what it backgrounded, when the step ends", func(ctx context.Context) {
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		pidFile := filepath.Join(GinkgoT().TempDir(), "child.pid")
		process, err := ws.SessionExec().Exec(ctx, workspace.Command{
			Path: "/bin/sh",
			Args: []string{"-c", `sleep 600 & echo $! > ` + pidFile + `
echo ready
while :; do sleep 0.1; done`},
		})
		Expect(err).NotTo(HaveOccurred())
		ready := make([]byte, 6)
		Expect(io.ReadFull(process.Stdout, ready)).To(Equal(6))
		Expect(string(ready)).To(Equal("ready\n"))
		raw, err := os.ReadFile(pidFile) //nolint:gosec // the spec's own scratch file
		Expect(err).NotTo(HaveOccurred())
		child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		Expect(err).NotTo(HaveOccurred())

		Expect(ws.EndStep(ctx, 1, true)).To(Succeed())

		Expect(io.Copy(io.Discard, process.Stdout)).Error().NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		process.Wait()

		Eventually(func() bool {
			return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
		}, time.Second, 20*time.Millisecond).Should(BeTrue(), "the backgrounded process outlived the step")
	})

	It("reaches a grandchild that detached into a session of its own, when this account has a systemd --user session", func(ctx context.Context) {
		probeUnit := "mill-spec-probe-" + strconv.Itoa(os.Getpid())
		probe := workspace.Spawner{}
		proc, err := probe.Exec(ctx, workspace.Command{Path: "systemd-run",
			Args: []string{"--user", "--scope", "--collect", "--unit=" + probeUnit, "--", "true"}})
		if err != nil {
			Skip("systemd-run: " + err.Error())
		}
		_ = proc.Stdin.Close()
		_, _ = io.Copy(io.Discard, proc.Stdout)
		_, _ = io.Copy(io.Discard, proc.Stderr)
		if proc.Wait().Status != 0 {
			Skip("no usable systemd --user session in this spec environment")
		}

		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		pidFile := filepath.Join(GinkgoT().TempDir(), "child.pid")
		process, err := ws.SessionExec().Exec(ctx, workspace.Command{
			Path: "/bin/sh",
			Args: []string{"-c", `setsid sleep 600 & echo $! > ` + pidFile + `
echo ready`},
		})
		Expect(err).NotTo(HaveOccurred())
		ready := make([]byte, 6)
		Expect(io.ReadFull(process.Stdout, ready)).To(Equal(6))
		Expect(io.Copy(io.Discard, process.Stdout)).Error().NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		process.Wait()
		raw, err := os.ReadFile(pidFile) //nolint:gosec // the spec's own scratch file
		Expect(err).NotTo(HaveOccurred())
		child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		Expect(err).NotTo(HaveOccurred())

		Expect(ws.EndStep(ctx, 1, true)).To(Succeed())

		Eventually(func() bool {
			return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
		}, 2*time.Second, 20*time.Millisecond).Should(BeTrue(), "the detached grandchild outlived the scope")
	})
})
