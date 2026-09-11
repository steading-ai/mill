package sweep_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSweep(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "sweep")
}
