package mill_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMill(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "development environment")
}
