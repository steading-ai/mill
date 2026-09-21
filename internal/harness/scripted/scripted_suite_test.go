package scripted_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestScripted(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "scripted harness")
}
