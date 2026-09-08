package claudecode

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestClaudecode(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "claudecode adapter")
}
