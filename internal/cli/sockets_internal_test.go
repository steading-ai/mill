package cli

import (
	"bytes"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
)

// The socket resolver: one order for every face that dials — the flag,
// then the variable, then the configuration file, then the default — and
// the file never above the environment, because every deployment that sets
// $MILL_SOCKET on a unit or a plugin registration relies on that.
var _ = Describe("The socket resolver", Label("unit"), func() {
	var config string

	BeforeEach(func() {
		config = filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(config, []byte("socket: /from/the/file.sock\nskift-socket: /from/the/file/skift.sock\n"), 0o600)).To(Succeed())
	})

	// dialing is a command declaring the two flags the way every face
	// does, with the file applied the way the root applies it.
	dialing := func(args ...string) (socket, skiftSocket string) {
		GinkgoHelper()
		cmd := &cobra.Command{Use: "dials"}
		cmd.Flags().StringVar(&socket, "socket", defaultSocketPath(), "")
		cmd.Flags().StringVar(&skiftSocket, "skift-socket", envOr(skiftSocketEnv, defaultSkiftSocket), "")
		Expect(cmd.ParseFlags(args)).To(Succeed())
		GinkgoT().Setenv(configEnv, config)
		Expect(applySocketConfig(cmd, nil)).To(Succeed())
		return socket, skiftSocket
	}

	It("reads both sockets from the file when the command line and the environment say nothing", func() {
		socket, skiftSocket := dialing()
		Expect(socket).To(Equal("/from/the/file.sock"))
		Expect(skiftSocket).To(Equal("/from/the/file/skift.sock"))
	})

	It("lets --socket and --skift-socket win over the file", func() {
		socket, skiftSocket := dialing("--socket", "/from/the/flag.sock", "--skift-socket", "/from/the/flag/skift.sock")
		Expect(socket).To(Equal("/from/the/flag.sock"))
		Expect(skiftSocket).To(Equal("/from/the/flag/skift.sock"))
	})

	It("lets $MILL_SOCKET and $SKIFT_SOCKET win over the file", func() {
		GinkgoT().Setenv(socketEnv, "/from/the/environment.sock")
		GinkgoT().Setenv(skiftSocketEnv, "/from/the/environment/skift.sock")
		socket, skiftSocket := dialing()
		Expect(socket).To(Equal("/from/the/environment.sock"))
		Expect(skiftSocket).To(Equal("/from/the/environment/skift.sock"))
	})

	It("lets a flag win over the environment", func() {
		GinkgoT().Setenv(socketEnv, "/from/the/environment.sock")
		socket, _ := dialing("--socket", "/from/the/flag.sock")
		Expect(socket).To(Equal("/from/the/flag.sock"))
	})

	It("leaves the default alone when the file names no socket", func() {
		Expect(os.WriteFile(config, []byte("concurrency: 1\n"), 0o600)).To(Succeed())
		socket, skiftSocket := dialing()
		Expect(socket).To(Equal(defaultSocketPath()))
		Expect(skiftSocket).To(Equal(defaultSkiftSocket))
	})

	It("reads the file --config names when the command declares one", func() {
		cmd := &cobra.Command{Use: "dials"}
		var socket, path string
		cmd.Flags().StringVar(&socket, "socket", defaultSocketPath(), "")
		cmd.Flags().StringVar(&path, "config", "", "")
		Expect(cmd.ParseFlags([]string{"--config", config})).To(Succeed())
		Expect(applySocketConfig(cmd, nil)).To(Succeed())
		Expect(socket).To(Equal("/from/the/file.sock"))
	})

	It("says nothing from a file that is not there, and refuses one that does not parse", func() {
		cmd := &cobra.Command{Use: "dials"}
		var socket string
		cmd.Flags().StringVar(&socket, "socket", defaultSocketPath(), "")
		GinkgoT().Setenv(configEnv, filepath.Join(GinkgoT().TempDir(), "absent.yaml"))
		Expect(applySocketConfig(cmd, nil)).To(Succeed())
		Expect(socket).To(Equal(defaultSocketPath()))

		Expect(os.WriteFile(config, []byte("socket: [unclosed\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv(configEnv, config)
		Expect(applySocketConfig(cmd, nil)).To(MatchError(ContainSubstring(config)))
	})

	// The resolver is the root's pre-run, so a reader with no --config of
	// its own reaches the file's socket: the error a `status` fails with
	// when nothing listens there names the file's path.
	It("reaches every face through the root: mill status dials the file's socket", func() {
		GinkgoT().Setenv(configEnv, config)
		var stdout, stderr bytes.Buffer
		root := NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"status"})
		err := root.Execute()
		Expect(err).To(MatchError(ContainSubstring("/from/the/file.sock")))
	})

	It("keeps work on the same resolver: the file's socket, unless $MILL_SOCKET says otherwise", func() {
		GinkgoT().Setenv(socketEnv, "/from/the/environment.sock")
		opts := workOptions{}
		cmd := workCommand(&opts)
		Expect(cmd.ParseFlags([]string{"--config", config})).To(Succeed())
		Expect(applyConfig(cmd, &opts)).To(Succeed())
		Expect(opts.run.socket).To(Equal("/from/the/environment.sock"))
		Expect(opts.run.skiftSocket).To(Equal("/from/the/file/skift.sock"), "the file fills what the environment left alone")
	})
})
