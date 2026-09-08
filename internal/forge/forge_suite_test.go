package forge_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestForge(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "forge")
}
