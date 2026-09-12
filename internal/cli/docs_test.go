package cli_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ivy/mill/internal/cli"
)

// docs/tools.md is the operator's document, and it is held to the command
// tree rather than maintained beside it: every command the tree has is
// described there under its own heading, and every flag a described
// command declares is named in the text. A command added without a section
// is a failing spec, as a config key added without its table row is.

const toolsDoc = "../../docs/tools.md"

// undocumented are the commands docs/tools.md deliberately leaves out:
// cobra's own, the faces that are not tools (docs/architecture.md §"One
// binary, two faces"), and `restart`, which docs/domain-model.md §Restart
// owns.
var undocumented = map[string]bool{
	"help":       true,
	"completion": true,
	"version":    true,
	"serve":      true,
	"work":       true,
	"run":        true,
	"restart":    true,
}

// commandPaths walks the tree and returns every command's path below the
// root, `cache ls` for a subcommand.
func commandPaths(cmd *cobra.Command) []string {
	var paths []string
	for _, child := range cmd.Commands() {
		if child.Hidden {
			continue
		}
		path := strings.TrimPrefix(child.CommandPath(), cmd.Root().Name()+" ")
		paths = append(paths, path)
		paths = append(paths, commandPaths(child)...)
	}
	return paths
}

// described reports whether the document has a section for a command: a
// `## <path>` heading, or — for a command that only groups subcommands,
// such as `cache` — a heading for any of them.
func described(doc, path string, groups bool) bool {
	if strings.Contains(doc, "\n## "+path+"\n") {
		return true
	}
	return groups && strings.Contains(doc, "\n## "+path+" ")
}

var _ = Describe("The tools document", Label("unit"), func() {
	var doc string
	var root *cobra.Command

	BeforeEach(func() {
		body, err := os.ReadFile(filepath.Clean(toolsDoc))
		Expect(err).NotTo(HaveOccurred())
		doc = string(body)
		root = cli.NewRootCommand(io.Discard, io.Discard)
	})

	// mill#70: the documents catch up with what was built.
	It("The tools are documented", func() {
		var missing []string
		By("every command the tree has is described, or is one the document deliberately leaves out")
		for _, path := range commandPaths(root) {
			top, _, _ := strings.Cut(path, " ")
			if undocumented[top] {
				continue
			}
			cmd, _, err := root.Find(strings.Fields(path))
			Expect(err).NotTo(HaveOccurred())
			if !described(doc, path, cmd.HasSubCommands()) {
				missing = append(missing, "mill "+path)
			}
		}
		Expect(missing).To(BeEmpty(), "docs/tools.md does not describe these commands")

		By("every flag a described command declares is named in the document")
		for _, path := range commandPaths(root) {
			top, _, _ := strings.Cut(path, " ")
			if undocumented[top] {
				continue
			}
			cmd, _, err := root.Find(strings.Fields(path))
			Expect(err).NotTo(HaveOccurred())
			visit := func(flag *pflag.Flag) {
				if flag.Name != "help" && !strings.Contains(doc, "--"+flag.Name) {
					missing = append(missing, "--"+flag.Name+" of mill "+path)
				}
			}
			cmd.LocalFlags().VisitAll(visit)
			cmd.InheritedFlags().VisitAll(visit)
		}
		Expect(missing).To(BeEmpty(), "docs/tools.md does not name these flags")
	})
})
