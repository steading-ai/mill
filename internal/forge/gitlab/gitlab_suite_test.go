package gitlab_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGitLab(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "forge/gitlab")
}
