// Package sidecar_test drives the TypeScript sidecar as the worker will: a
// child process, one harness.v1 request on its stdin, its events and
// terminal record on its stdout.
//
// The specs live here rather than beside the Go adapter because the sidecar
// is the code under test and this is the package it lives in. What they
// exercise is the sidecar's own behaviour — the framing, the exit mapping,
// the usage accumulation, the environment a session is handed — which is
// deterministic under a fake harness and does not need a credential. The
// specs that do need one are labelled `live` and skip unless the environment
// asks for them.
package sidecar_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The sidecar is compiled once, in the first process, before any spec runs.
var _ = SynchronizedBeforeSuite(func() []byte {
	build()
	return nil
}, func([]byte) {})

func TestSidecar(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "the Claude Agent SDK sidecar")
}
