package harnessv1_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestHarnessV1(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "harness.v1 wire")
}
