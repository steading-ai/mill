package harnessv1_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
)

// repoRoot is three directories up from gen/harness/v1, which is where
// Ginkgo runs this package's specs from.
const repoRoot = "../../.."

// sidecarDir holds the TypeScript half of the wire: the generated types, the
// round-trip script the specs drive, and the lockfile that pins both.
var sidecarDir = filepath.Join(repoRoot, "sidecar")

// roundTripThroughTypeScript feeds JSON to the generated TypeScript type and
// returns what it emits after parsing and re-serializing.
func roundTripThroughTypeScript(in []byte) []byte {
	GinkgoHelper()

	cmd := exec.Command("node", "dist/spec/terminal_roundtrip.js")
	cmd.Dir = sidecarDir
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stderr = GinkgoWriter

	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred(), "node could not parse the Terminal")
	return out
}

// pnpm runs one of the sidecar's package scripts, failing the spec with its
// output when it does not succeed.
func pnpm(args ...string) {
	GinkgoHelper()

	cmd := exec.Command("pnpm", args...)
	cmd.Dir = sidecarDir
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed(), "pnpm %v", args)
}

var _ = Describe("The sidecar wire", func() {
	// The TypeScript half is compiled once for the suite: the generated
	// types are the thing under test, so they are exercised as the sidecar
	// will run them rather than through a type-stripping shortcut.
	BeforeEach(func() {
		if _, err := os.Stat(filepath.Join(sidecarDir, "node_modules")); os.IsNotExist(err) {
			pnpm("install", "--frozen-lockfile")
		}
		pnpm("build")
	})

	It("The wire is typed on both sides", func() {
		By("given a Terminal message constructed in Go and serialized as JSON")
		structured, err := structpb.NewStruct(map[string]any{
			"decision": "approve",
			"findings": []any{map[string]any{"criterion": "AC-2", "severity": "note"}},
		})
		Expect(err).NotTo(HaveOccurred())

		terminal := &harnessv1.Terminal{
			RequestId:        "mill-01JB-implement-1",
			FinalMessage:     "Implemented the step and pushed the branch.",
			StructuredOutput: structured,
			Usage: &harnessv1.Usage{
				ByModel: map[string]*harnessv1.TokenCounts{
					"claude-opus-5": {
						Input:      1201,
						Output:     3405,
						CacheRead:  88_000,
						CacheWrite: 12_500,
					},
					"claude-haiku-4-5": {
						Input:      42,
						Output:     7,
						CacheRead:  1,
						CacheWrite: 2,
					},
				},
			},
			Exit: &harnessv1.ProcessExit{
				Status: 137,
				Signal: 9,
				Reason: harnessv1.ExitReason_EXIT_REASON_TIMEOUT,
			},
		}
		encoded, err := protojson.Marshal(terminal)
		Expect(err).NotTo(HaveOccurred())

		By("when the TypeScript generated type parses it")
		emitted := roundTripThroughTypeScript(encoded)

		By("then every field round-trips including the per-model usage map")
		var got harnessv1.Terminal
		Expect(protojson.Unmarshal(emitted, &got)).To(Succeed())
		Expect(proto.Equal(&got, terminal)).To(BeTrue(),
			"TypeScript emitted %s", emitted)

		// Field by field, so a failure names what was lost rather than
		// only that something was.
		Expect(got.GetRequestId()).To(Equal(terminal.GetRequestId()))
		Expect(got.GetFinalMessage()).To(Equal(terminal.GetFinalMessage()))
		Expect(got.GetStructuredOutput().AsMap()).To(Equal(structured.AsMap()))
		Expect(got.GetExit().GetStatus()).To(BeEquivalentTo(137))
		Expect(got.GetExit().GetSignal()).To(BeEquivalentTo(9))
		Expect(got.GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_TIMEOUT))

		Expect(got.GetUsage().GetByModel()).To(HaveLen(2))
		for model, want := range terminal.GetUsage().GetByModel() {
			counts := got.GetUsage().GetByModel()[model]
			Expect(counts).NotTo(BeNil(), "model %s was dropped", model)
			Expect(counts.GetInput()).To(Equal(want.GetInput()), "model %s", model)
			Expect(counts.GetOutput()).To(Equal(want.GetOutput()), "model %s", model)
			Expect(counts.GetCacheRead()).To(Equal(want.GetCacheRead()), "model %s", model)
			Expect(counts.GetCacheWrite()).To(Equal(want.GetCacheWrite()), "model %s", model)
		}

		// The map survived as a map, not as a re-encoded blob: the keys
		// TypeScript emitted are the model names Go sent.
		var raw map[string]any
		Expect(json.Unmarshal(emitted, &raw)).To(Succeed())
		Expect(raw).To(HaveKey("usage"))
		Expect(raw["usage"]).To(HaveKey("byModel"))
		Expect(raw["usage"].(map[string]any)["byModel"]).To(
			HaveKey("claude-opus-5"))
	})
})
