package socket_test

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/socket"
)

var _ = Describe("A socket path the kernel cannot hold", Label("unit"), func() {
	// One byte over is the whole case: the kernel answers "bind: invalid
	// argument", which names neither the path nor the length nor the limit.
	longPath := func(over int) string {
		return "/tmp/" + strings.Repeat("x", socket.MaxPathLen+over-len("/tmp/"))
	}

	It("passes a path that fits", func() {
		Expect(socket.CheckPath(longPath(0))).To(Succeed())
	})

	It("refuses a path one byte over the limit, naming the path, its length and the limit", func() {
		path := longPath(1)

		err := socket.CheckPath(path)

		var tooLong *socket.PathTooLongError
		Expect(errors.As(err, &tooLong)).To(BeTrue(), "%v", err)
		Expect(tooLong.Path).To(Equal(path))
		Expect(tooLong.Limit).To(Equal(socket.MaxPathLen))
		Expect(err.Error()).To(ContainSubstring(path))
		Expect(err.Error()).To(ContainSubstring(strconv.Itoa(len(path))))
		Expect(err.Error()).To(ContainSubstring(strconv.Itoa(socket.MaxPathLen)))
	})

	It("refuses to listen on one, rather than failing on the bind", func() {
		server := socket.New(nil)
		path := filepath.Join(GinkgoT().TempDir(), strings.Repeat("d", socket.MaxPathLen), "ws.sock")

		err := server.Listen(path)

		var tooLong *socket.PathTooLongError
		Expect(errors.As(err, &tooLong)).To(BeTrue(), "%v", err)
		Expect(err.Error()).NotTo(ContainSubstring("invalid argument"))
	})
})
