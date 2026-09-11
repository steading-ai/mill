// Package nspawn_test holds the executable form of the scenarios in
// docs/spikes/nspawn-per-run-sandbox.md: a real systemd-nspawn machine booted
// from /var/lib/machines/mill-image-spike on a host with root, nspawn and
// btrfs. The specs are the live tier of docs/sandbox.md §Testing and skip
// unless MILL_SANDBOX_HOST is set.
package nspawn_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNspawn(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "nspawn spike")
}
