package verbs_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestVerbs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "workspace/verbs")
}
