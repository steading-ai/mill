package doctor

import (
	"net"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("checkSessionEnv, against a fabricated environment", Label("unit"), func() {
	It("fails when SKIFT_SOCKET is missing", func() {
		result := checkSessionEnv(map[string]string{"MILL_SOCKET": "/tmp/dead-mill.sock"}, "/run/mill/ws.sock", "/run/skift/skift.sock")
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("SKIFT_SOCKET"))
	})

	It("fails when MILL_SOCKET is missing", func() {
		result := checkSessionEnv(map[string]string{"SKIFT_SOCKET": "/tmp/dead-skift.sock"}, "/run/mill/ws.sock", "/run/skift/skift.sock")
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("MILL_SOCKET"))
	})

	It("fails when a socket names the production path", func() {
		result := checkSessionEnv(map[string]string{
			"SKIFT_SOCKET": "/run/skift/skift.sock",
			"MILL_SOCKET":  "/tmp/dead-mill.sock",
		}, "/run/mill/ws.sock", "/run/skift/skift.sock")
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("/run/skift/skift.sock"))
	})

	It("fails when a dial to the named socket succeeds", func() {
		live := filepath.Join(GinkgoT().TempDir(), "live.sock")
		listener, err := net.Listen("unix", live)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(listener.Close()).To(Succeed()) })

		result := checkSessionEnv(map[string]string{
			"SKIFT_SOCKET": live,
			"MILL_SOCKET":  "/tmp/dead-mill.sock",
		}, "/run/mill/ws.sock", "/run/skift/skift.sock")
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring(live))
	})

	It("passes when both are dead, distinct paths", func() {
		result := checkSessionEnv(map[string]string{
			"SKIFT_SOCKET": "/tmp/dead-skift.sock",
			"MILL_SOCKET":  "/tmp/dead-mill.sock",
		}, "/run/mill/ws.sock", "/run/skift/skift.sock")
		Expect(result.OK).To(BeTrue(), result.Reason)
	})
})
