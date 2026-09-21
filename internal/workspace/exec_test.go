package workspace_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
)

// script writes a shell script into a temporary directory and returns its
// path.
func script(body string) string {
	GinkgoHelper()

	path := filepath.Join(GinkgoT().TempDir(), "stub.sh")
	Expect(os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700)).To(Succeed()) //nolint:gosec // a spec's own scratch script
	return path
}

var _ = Describe("The process spawner", Label("unit"), func() {
	var local workspace.Spawner

	BeforeEach(func() { local = workspace.Spawner{} })

	It("runs a command and hands back its output and status", func(ctx context.Context) {
		process, err := local.Exec(ctx, workspace.Command{
			Path: "/bin/sh", Args: []string{script(`echo out; echo err >&2; exit 3`)},
		})
		Expect(err).NotTo(HaveOccurred())

		stdout, err := io.ReadAll(process.Stdout)
		Expect(err).NotTo(HaveOccurred())
		stderr, err := io.ReadAll(process.Stderr)
		Expect(err).NotTo(HaveOccurred())
		result := process.Wait()

		Expect(string(stdout)).To(Equal("out\n"))
		Expect(string(stderr)).To(Equal("err\n"))
		Expect(result).To(Equal(workspace.Result{Status: 3}))
	})

	It("adds the command's own variables over the floor it inherits", func(ctx context.Context) {
		local.Floor = []string{"PATH=/usr/bin:/bin", "HOME=/home/mill", "STEP_TOKEN=stale"}

		process, err := local.Exec(ctx, workspace.Command{
			Path: "/bin/sh", Args: []string{script(`printf '%s\n' "$HOME" "$PATH" "$STEP_TOKEN" "$WORKSPACE_SOCKET" "[$MILL_NOT_ON_THE_FLOOR]"`)},
			Env: map[string]string{"STEP_TOKEN": "fresh", "WORKSPACE_SOCKET": "/run/mill/ws.sock"},
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := io.ReadAll(process.Stdout)
		Expect(err).NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		Expect(process.Wait().Status).To(BeZero())

		Expect(strings.Split(strings.TrimSpace(string(out)), "\n")).To(Equal([]string{
			"/home/mill",
			"/usr/bin:/bin",
			"fresh",
			"/run/mill/ws.sock",
			"[]",
		}))
	})

	It("runs the command in a working directory of the caller's choosing", func(ctx context.Context) {
		dir := GinkgoT().TempDir()

		process, err := local.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{script(`pwd`)}, Dir: dir})
		Expect(err).NotTo(HaveOccurred())
		out, err := io.ReadAll(process.Stdout)
		Expect(err).NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		process.Wait()

		resolved, err := filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(out))).To(Equal(resolved))
	})

	It("signals the whole process group, so nothing the child spawned outlives it", func(ctx context.Context) {
		pidFile := filepath.Join(GinkgoT().TempDir(), "child.pid")
		process, err := local.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{script(`
sleep 600 &
echo $! > ` + pidFile + `
echo ready
while :; do sleep 0.1; done
`)}})
		Expect(err).NotTo(HaveOccurred())
		ready := make([]byte, 6)
		Expect(io.ReadFull(process.Stdout, ready)).To(Equal(6))
		raw, err := os.ReadFile(pidFile) //nolint:gosec // a spec's own scratch file
		Expect(err).NotTo(HaveOccurred())
		child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		Expect(err).NotTo(HaveOccurred())

		Expect(process.Signal(syscall.SIGKILL)).To(Succeed())

		Expect(io.Copy(io.Discard, process.Stdout)).Error().NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		Expect(process.Wait().Signal).To(BeNumerically("==", 9))
		Eventually(func() bool {
			return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
		}, time.Second, 20*time.Millisecond).Should(BeTrue(), "the child outlived its group")
	})

	It("reports a signal to a group that is already gone as the outcome the caller wanted", func(ctx context.Context) {
		process, err := local.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{script(`exit 0`)}})
		Expect(err).NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stdout)).Error().NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		process.Wait()

		Expect(process.Signal(syscall.SIGTERM)).To(Succeed())
	})

	It("refuses a command with nothing to run, and a context that is already done", func(ctx context.Context) {
		_, err := local.Exec(ctx, workspace.Command{})
		Expect(err).To(MatchError(ContainSubstring("no command to run")))

		done, cancel := context.WithCancel(ctx)
		cancel()
		_, err = local.Exec(done, workspace.Command{Path: "/bin/sh"})
		Expect(err).To(MatchError(context.Canceled))
	})
})

var _ = Describe("The dead sockets a workspace hands a child", Label("unit"), func() {
	It("are two distinct, absolute paths under the run's own directory", func() {
		dir := GinkgoT().TempDir()
		env := workspace.DeadSockets(dir)

		Expect(env).To(HaveKey("DG_SOCKET"))
		Expect(env).To(HaveKey("MILL_SOCKET"))
		Expect(env["DG_SOCKET"]).NotTo(Equal(env["MILL_SOCKET"]))
		for name, path := range env {
			Expect(filepath.IsAbs(path)).To(BeTrue(), "%s is not absolute: %s", name, path)
			Expect(strings.HasPrefix(path, dir+string(filepath.Separator))).To(BeTrue(), "%s is not under %s: %s", name, dir, path)
			Expect(path).NotTo(Equal("/run/dagger/dg.sock"))
			Expect(path).NotTo(Equal("/run/mill/mill.sock"))
			Expect(path).NotTo(MatchRegexp(`^/run/user/\d+/mill/mill\.sock$`))
		}
	})

	It("names paths nothing is listening on", func() {
		env := workspace.DeadSockets(GinkgoT().TempDir())
		for name, path := range env {
			_, err := net.DialTimeout("unix", path, 100*time.Millisecond)
			Expect(err).To(HaveOccurred(), "%s: something answers at %s", name, path)
		}
	})
})
