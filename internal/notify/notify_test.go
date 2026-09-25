package notify_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/notify"
)

// script writes a shell script into a temporary directory and returns its
// path. A notify command is somebody else's script, so the specs use one.
func script(body string) string {
	GinkgoHelper()
	path := filepath.Join(GinkgoT().TempDir(), "notify.sh")
	Expect(os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700)).To(Succeed()) //nolint:gosec // a spec's own scratch script
	return path
}

var _ = Describe("The notify command", Label("unit"), func() {
	notice := notify.Notice{
		Kind:  notify.KindPlan,
		Gate:  "mill#41",
		Node:  "mill#33",
		Run:   "01JRUN",
		Title: "Approve the plan for mill#33?",
		Body:  "The plan is attached to this node.",
	}

	It("hands the notice to the command as JSON on its standard input", func(ctx context.Context) {
		out := filepath.Join(GinkgoT().TempDir(), "notice.json")
		command := &notify.Command{Path: script("cat > " + out)}

		result, err := command.Notify(ctx, notice)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.OK()).To(BeTrue(), "%s", result)

		raw, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		var decoded map[string]any
		Expect(json.Unmarshal(raw, &decoded)).To(Succeed())
		Expect(decoded).To(Equal(map[string]any{
			"kind":  "plan",
			"gate":  "mill#41",
			"node":  "mill#33",
			"run":   "01JRUN",
			"title": "Approve the plan for mill#33?",
			"body":  "The plan is attached to this node.",
		}))
	})

	It("reports a command that failed as its exit status and its diagnostic", func(ctx context.Context) {
		command := &notify.Command{Path: script("echo 'no agent named ops' >&2; exit 3")}

		result, err := command.Notify(ctx, notice)
		Expect(err).NotTo(HaveOccurred(), "a command that ran and failed is a fact, not an error")
		Expect(result.OK()).To(BeFalse())
		Expect(result.Status).To(Equal(int32(3)))
		Expect(result.Detail).To(ContainSubstring("no agent named ops"))
	})

	It("reports a command that is not there rather than failing", func(ctx context.Context) {
		command := &notify.Command{Path: filepath.Join(GinkgoT().TempDir(), "nothing")}

		result, err := command.Notify(ctx, notice)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Status).To(Equal(int32(-1)))
		Expect(result.Detail).NotTo(BeEmpty())
	})

	It("kills a command that outlasts its bound and says so", func(ctx context.Context) {
		command := &notify.Command{Path: script("sleep 60"), Timeout: 200 * time.Millisecond}

		started := time.Now()
		result, err := command.Notify(ctx, notice)
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Since(started)).To(BeNumerically("<", 10*time.Second))
		Expect(result.Status).To(Equal(int32(-1)))
		Expect(result.Detail).To(ContainSubstring("did not answer in time"))
	})

	It("bounds a notification at thirty seconds when nothing says otherwise", func() {
		Expect(notify.Timeout).To(Equal(30 * time.Second))
	})

	It("runs even when the caller's context is already done: the gate is filed either way", func() {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		out := filepath.Join(GinkgoT().TempDir(), "notice.json")

		result, err := (&notify.Command{Path: script("cat > " + out)}).Notify(cancelled, notice)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.OK()).To(BeTrue(), "%s", result)
		Expect(out).To(BeAnExistingFile())
	})

	Describe("Parse", func() {
		It("splits a command and its arguments, and reads nothing as no notifier at all", func() {
			Expect(notify.Parse("")).To(BeNil())
			Expect(notify.Parse("   ")).To(BeNil())

			parsed := notify.Parse("/usr/bin/notify --agent ops")
			Expect(parsed.Path).To(Equal("/usr/bin/notify"))
			Expect(parsed.Args).To(Equal([]string{"--agent", "ops"}))
		})
	})
})
