package testsupport

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace"
)

// lookPath finds a program on PATH. It is exec.LookPath's job, which this
// package may not call: os/exec lives in internal/workspace alone, and the
// search itself reads the filesystem rather than spawning anything.
func lookPath(name string) (string, error) {
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

// A real skift for the run loop's specs.
//
// The loop's contract with skift is fencing: a claim under a lease, a
// heartbeat that fails when the lease is gone, a patch refused when it is
// not current. None of that behaves like production against a fake, so the
// specs run the real service — `skift serve` from the operator's own binary,
// against a database of its own from the same container the mill's specs
// use.
//
// Skift publishes no service image (its compose.yaml carries PostgreSQL
// alone), so the binary and its migrations come from the checkout the design
// documents name. A host without them skips rather than fails: the fixture
// says what is missing.

// SkiftRepoEnv overrides where skift's checkout is looked for.
const SkiftRepoEnv = "MILL_SKIFT_REPO"

// defaultSkiftRepo is where the operator keeps checkouts.
const defaultSkiftRepo = "src/github.com/steading-ai/skift"

// skiftReady bounds how long the service has to bind its socket.
const skiftReady = 30 * time.Second

// Skift is a running skift service and what a spec needs to reach it.
type Skift struct {
	// Socket is the unix socket the service listens on.
	Socket string
	// Org is the organization the seeded principal belongs to.
	Org string
	// Principal is the seeded `user` principal, which the spec process's own
	// uid resolves to — the same arrangement mill runs under in production
	// (docs/domain-model.md §Principal).
	Principal string
	// DatabaseURL is skift's own database, for the few facts its API does
	// not expose. A released lease is one: skift returns the live lease on
	// a node and nothing about the one before it, so a spec asserting why a
	// lease ended reads the row.
	DatabaseURL string
}

// StartSkift brings up a skift service for one spec: an empty database
// from the shared container, skift's own migrations applied to it, an
// organization and a principal for this process's uid, and `skift serve` bound
// to a socket in the spec's temporary directory. Everything is torn down
// when the spec ends.
//
// The spec must carry [DBLabel]; the database is the container's.
func (p *Postgres) StartSkift(ctx context.Context) *Skift {
	ginkgo.GinkgoHelper()
	requireAvailable()
	return p.startSkift(ctx, p.Blank(ctx))
}

// StartSuiteSkift brings up one skift service for a whole suite, from the
// first function of a SynchronizedBeforeSuite: the same empty database,
// migrations, seed and `skift serve` as [Postgres.StartSkift], torn down when
// the suite ends rather than when a spec does. It is what a suite that
// files real nodes uses instead of the operator's deployed skift, so a
// suite that is killed rather than finished leaves its leftovers in a
// database the container takes with it and nothing in a graph anyone
// reads (docs/runbook.md §"What it leaves behind").
//
// A suite setup node has no labels, so [DBLabel] is not asked for; the
// suite that calls this has already decided to start the container.
func (p *Postgres) StartSuiteSkift(ctx context.Context) *Skift {
	ginkgo.GinkgoHelper()
	requireAvailable()
	return p.startSkift(ctx, p.blank(ctx))
}

// SkiftUnavailable is why this host cannot start a skift, or empty when
// it can: the checkout the migrations are read from, the `skift` binary, and
// `goose` to apply them. A suite that refuses before it spends anything
// asks this in its preflight.
func SkiftUnavailable() string {
	var missing []string
	if skiftRepo() == "" {
		missing = append(missing, "no skift checkout: set "+SkiftRepoEnv+" to a checkout of github.com/steading-ai/skift")
	}
	if skiftBinary() == "" {
		missing = append(missing, "`skift` is neither on PATH nor in the checkout")
	}
	if _, err := lookPath("goose"); err != nil {
		missing = append(missing, "`goose` is not on PATH")
	}
	if len(missing) == 0 {
		return ""
	}
	return "skift cannot be started on this host: " + strings.Join(missing, "; ")
}

// requireAvailable skips the caller when this host cannot start a skift,
// naming what is missing.
func requireAvailable() {
	ginkgo.GinkgoHelper()
	if reason := SkiftUnavailable(); reason != "" {
		ginkgo.Skip(reason)
	}
}

// startSkift migrates and seeds databaseURL and starts `skift serve` on a
// socket of its own. Both are torn down by the cleanup of whichever node
// called it.
func (p *Postgres) startSkift(ctx context.Context, databaseURL string) *Skift {
	ginkgo.GinkgoHelper()
	repo := skiftRepo()
	binary := skiftBinary()

	migrate, err := spawner.Exec(ctx, workspace.Command{
		Path: "goose",
		Args: []string{
			"-dir", filepath.Join(repo, "internal", "store", "migrations"),
			"postgres", databaseURL, "up",
		},
		Dir: repo,
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	diagnostics := drain(migrate.Stderr)
	gomega.Expect(migrate.Wait().Status).To(gomega.BeZero(), "applying skift's migrations: %s", diagnostics())

	d := &Skift{
		Org:         "01HZZZZZZZZZZZZZZZZZZZZZZ0",
		Principal:   "01HZZZZZZZZZZZZZZZZZZZZZZ1",
		DatabaseURL: databaseURL,
	}
	seedSkift(ctx, databaseURL, d)

	dir, err := os.MkdirTemp("", "mill-skift-*")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(func() error { return os.RemoveAll(dir) })
	d.Socket = filepath.Join(dir, "skift.sock")

	serve, err := spawner.Exec(ctx, workspace.Command{
		Path: binary,
		Args: []string{"serve", "--socket", d.Socket, "--database-url", databaseURL, "--log-level", "warn"},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// Both pipes are drained. A pipe nobody reads fills at 64KiB and blocks
	// the writer, which for a service under a spec that makes hundreds of
	// requests means skift stops answering halfway through.
	go func() { _, _ = io.Copy(ginkgo.GinkgoWriter, serve.Stderr) }()
	go func() { _, _ = io.Copy(ginkgo.GinkgoWriter, serve.Stdout) }()
	ginkgo.DeferCleanup(func() {
		_ = serve.Signal(syscall.SIGKILL)
		serve.Wait()
	})

	gomega.Eventually(func() error {
		conn, err := net.Dial("unix", d.Socket)
		if err != nil {
			return err
		}
		return conn.Close()
	}, skiftReady, 50*time.Millisecond).Should(gomega.Succeed())
	return d
}

// HTTPClient dials the service's socket. Connect needs a well-formed URL to
// build a path from; the host is never resolved.
func (d *Skift) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.Socket)
		},
	}}
}

// BaseURL is what a Connect client is built against.
func (d *Skift) BaseURL() string { return "http://skift" }

// seedSkift inserts the organization, the principal, and the unix identity
// this process's uid resolves to. Skift has no bootstrap command; its own
// acceptance suite seeds the same three rows.
func seedSkift(ctx context.Context, databaseURL string, d *Skift) {
	ginkgo.GinkgoHelper()
	conn, err := pgx.Connect(ctx, databaseURL)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // seeding connection; nothing to flush

	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name) VALUES ($1, $2)`, []any{d.Org, "mill-specs"}},
		{`INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, 'user')`, []any{d.Principal, d.Org}},
		{`INSERT INTO principal_identities (principal_id, scheme, value) VALUES ($1, 'unix', $2)`,
			[]any{d.Principal, strconv.Itoa(os.Getuid())}},
	}
	for _, s := range statements {
		_, err := conn.Exec(ctx, s.sql, s.args...)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "seeding skift: %s", s.sql)
	}
}

// spawner is the process chokepoint, which the fixture goes through for the
// same reason everything else does: os/exec has exactly one home in this
// tree (docs/architecture.md §"The workspace").
var spawner = workspace.Spawner{}

// drain reads a pipe in the background and returns what it has when asked.
func drain(reader io.Reader) func() string {
	var mu sync.Mutex
	var buf []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		read, _ := io.ReadAll(reader)
		mu.Lock()
		buf = read
		mu.Unlock()
	}()
	return func() string {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return string(buf)
	}
}

// skiftRepo is the checkout skift's migrations are read from, or empty.
func skiftRepo() string {
	candidates := []string{os.Getenv(SkiftRepoEnv)}
	if home, err := user.Current(); err == nil {
		candidates = append(candidates, filepath.Join(home.HomeDir, defaultSkiftRepo))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate, "internal", "store", "migrations")); err == nil {
			return candidate
		}
	}
	return ""
}

// skiftBinary is `skift`, on PATH or in the checkout.
func skiftBinary() string {
	if path, err := lookPath("skift"); err == nil {
		return path
	}
	if repo := skiftRepo(); repo != "" {
		candidate := filepath.Join(repo, "skift")
		if info, err := os.Stat(candidate); err == nil && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}
