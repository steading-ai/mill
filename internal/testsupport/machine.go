package testsupport

import (
	"context"
	"os"
	"strings"

	"github.com/onsi/ginkgo/v2"

	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// containerFile is where systemd, as a machine's PID 1, records the manager
// that started it; `systemd-detect-virt --container` reads the same file.
const containerFile = "/run/systemd/container"

// nspawn is the file's content inside a systemd-nspawn machine.
const nspawn = "systemd-nspawn"

// InsideMachine reports whether this process runs inside a machine — a
// systemd-nspawn container, which is where mill's own live tier runs once
// mill builds mill (docs/sandbox.md §Self-hosting). A live spec that boots
// a machine through the host's template unit, builds an image on the host's
// volume or needs the host's Harbor asks this and skips, naming what it
// needs; one written for the inside of a machine — a lineage under the
// nested worker's root — runs.
func InsideMachine() bool {
	content, err := os.ReadFile(containerFile)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(content)) == nspawn
}

// HostOnlySkip is the message a live spec skips with inside a machine: what
// the spec needs of the host and why a machine has none of it.
func HostOnlySkip(needs string) string {
	return "inside a machine: this spec needs " + needs + ", which a machine has none of (docs/sandbox.md §Self-hosting); it runs on the host"
}

// SkipUnlessSandboxReady skips the running spec, naming the first thing
// host.SandboxReady reports missing (or the error it hit checking).
func SkipUnlessSandboxReady(ctx context.Context, host driver.Driver, root, machines string) {
	ginkgo.GinkgoHelper()
	reason, err := host.SandboxReady(ctx, root, machines)
	if err != nil {
		reason = err.Error()
	}
	if reason != "" {
		ginkgo.Skip(reason)
	}
}
