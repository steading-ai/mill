package cli_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/sdnotify"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace/socket"
)

// notifier stands in for systemd: a datagram socket named by NOTIFY_SOCKET,
// whose lines the service sends its readiness on.
func notifier(dir string) chan string {
	GinkgoHelper()
	path := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	GinkgoT().Setenv(sdnotify.Env, path)

	states := make(chan string, 8)
	go func() {
		defer GinkgoRecover()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				close(states)
				return
			}
			for _, line := range strings.Split(strings.TrimSpace(string(buf[:n])), "\n") {
				states <- line
			}
		}
	}()
	return states
}

// unixClient dials the socket whatever the URL says.
func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

var _ = Describe("mill serve", Label(testsupport.DBLabel), func() {
	It("binds its socket, migrates, answers Status, and reports ready once the database has answered", func(ctx SpecContext) {
		db := pg.Database(ctx)
		databaseURL := db.Config().ConnString()

		// The database this spec hands the service has no schema: the
		// template it was cloned from was left empty on purpose.
		var tables int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).
			Scan(&tables)).To(Succeed())
		Expect(tables).To(BeZero())

		dir := GinkgoT().TempDir()
		socket := filepath.Join(dir, "o.sock")
		states := notifier(dir)

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"serve", "--socket", socket, "--database-url", databaseURL})

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			served <- root.ExecuteContext(serveCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(served).Should(Receive(BeNil()), stderr.String())
		})

		// Readiness is reported, and only once the database has answered:
		// the migrations it applied are in the database by the time the
		// line arrives.
		Eventually(states).Should(Receive(Equal(sdnotify.Ready)), stderr.String())
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).
			Scan(&tables)).To(Succeed())
		Expect(tables).To(BeNumerically(">", 1), "the service migrated the database before reporting ready")

		info, err := os.Stat(socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode() & os.ModeSocket).NotTo(BeZero())

		client := millv1connect.NewStoreServiceClient(unixClient(socket), "http://mill")
		listed, err := client.Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listed.Msg.GetRuns()).To(BeEmpty(), "an empty ledger has no runs to report")
	})

	// A socket path over the kernel's limit fails on the bind as "invalid
	// argument", and by then the migrations have run for a service that
	// will not start. It is refused first, and the refusal says by how much
	// the path is too long.
	It("refuses a socket path the kernel cannot bind, before it migrates or binds", func(ctx SpecContext) {
		var stdout, stderr bytes.Buffer
		path := filepath.Join(GinkgoT().TempDir(), strings.Repeat("s", socket.MaxPathLen), "mill.sock")
		root := cli.NewRootCommand(&stdout, &stderr)
		// A database URL nothing serves: reaching it at all would be the
		// failure this refusal comes before.
		root.SetArgs([]string{"serve", "--socket", path,
			"--database-url", "postgres://nobody@127.0.0.1:1/none"})

		err := root.ExecuteContext(ctx)

		Expect(err).To(MatchError(ContainSubstring(path)))
		Expect(err).To(MatchError(ContainSubstring(strconv.Itoa(len(path)))))
		Expect(err).To(MatchError(ContainSubstring(strconv.Itoa(socket.MaxPathLen))))
		Expect(err).To(MatchError(ContainSubstring("--socket")))
		Expect(err).NotTo(MatchError(ContainSubstring("migrate")))
	})

	It("refuses to start without a database URL", func(ctx SpecContext) {
		GinkgoT().Setenv("DATABASE_URL", "")
		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"serve", "--socket", filepath.Join(GinkgoT().TempDir(), "o.sock")})
		Expect(root.ExecuteContext(ctx)).To(MatchError(ContainSubstring("database URL is required")))
	})
})
