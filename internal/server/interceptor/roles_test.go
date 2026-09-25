package interceptor_test

import (
	"os"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/server/interceptor"
)

// write puts content in a file of the spec's own directory and returns its
// path.
func write(content string) string {
	GinkgoHelper()
	path := filepath.Join(GinkgoT().TempDir(), "roles.json")
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
	return path
}

// roleOf is the role a uid holds, asserting that it holds one.
func roleOf(roles interceptor.RoleMap, uid int) interceptor.Role {
	GinkgoHelper()
	role, ok := roles.Role(uid)
	Expect(ok).To(BeTrue(), "uid %d holds no role", uid)
	return role
}

var _ = Describe("The role map", Label("unit"), func() {
	It("is the service's own uid and nothing else when no file is configured", func() {
		roles, err := interceptor.LoadRoleMap("")
		Expect(err).NotTo(HaveOccurred())
		Expect(roles.ServiceUID).To(Equal(os.Getuid()))

		Expect(roleOf(roles, os.Getuid())).To(Equal(interceptor.RoleService))

		_, ok := roles.Role(os.Getuid() + 1)
		Expect(ok).To(BeFalse())
	})

	It("maps each uid in the file to its role", func() {
		roles, err := interceptor.LoadRoleMap(write(`{"operator": [4001], "reader": [4002, 4003]}`))
		Expect(err).NotTo(HaveOccurred())

		Expect(roleOf(roles, 4001)).To(Equal(interceptor.RoleOperator))
		Expect(roleOf(roles, 4002)).To(Equal(interceptor.RoleReader))
		Expect(roleOf(roles, 4003)).To(Equal(interceptor.RoleReader))

		_, ok := roles.Role(4004)
		Expect(ok).To(BeFalse(), "a uid the file does not name holds no role")
	})

	It("keeps the service's own uid whatever the file says", func() {
		roles, err := interceptor.LoadRoleMap(write(`{"reader": [` + strconv.Itoa(os.Getuid()) + `]}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(roleOf(roles, os.Getuid())).To(Equal(interceptor.RoleService),
			"the account the service runs as is the one that writes its own ledger")
	})

	It("refuses a name that is not a role", func() {
		_, err := interceptor.LoadRoleMap(write(`{"admin": [4001]}`))
		Expect(err).To(MatchError(ContainSubstring("admin")))
	})

	It("refuses a file that is not there", func() {
		_, err := interceptor.LoadRoleMap(filepath.Join(GinkgoT().TempDir(), "absent.json"))
		Expect(err).To(HaveOccurred())
	})
})
