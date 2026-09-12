// Package testsupport holds the fixtures shared by every Ginkgo suite.
//
// PostgreSQL is the one that matters. The ledger's resume algorithm, step
// tokens and claims under concurrency only behave like production against a
// real server, so specs that touch the database run against one in a
// container and nothing is faked.
package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/wait"
)

// postgresImage is the same pinned postgres:18 the development stack in
// compose.yaml runs, so specs and `make db-up` see one server version.
const postgresImage = "docker.io/library/postgres:18@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280"

// DBLabel marks a spec as needing the container. `make test-unit` filters it
// out; `make test-db` runs nothing else.
const DBLabel = "db"

const (
	superuser    = "postgres"
	password     = "postgres"
	templateName = "mill_template"
	// startTimeout covers a cold image pull with margin.
	startTimeout = 3 * time.Minute
)

// Migrate brings an empty database up to the current schema. It runs once,
// against the template every spec database is cloned from, and receives a
// connection URL so any migration tool can be plugged in.
type Migrate func(ctx context.Context, databaseURL string) error

// Postgres is one server shared by every spec in a suite. Ginkgo runs
// parallel specs in separate processes, so the container is started by the
// first process and the others attach to it by URL: see [Start] and [Attach].
type Postgres struct {
	container testcontainers.Container
	adminURL  string
}

// Start pulls the image if needed, starts a server, creates the template
// database and applies migrate to it. A nil migrate leaves the template
// empty. Call it once per suite, from the first process of a
// SynchronizedBeforeSuite.
func Start(ctx context.Context, migrate Migrate) (*Postgres, error) {
	useRootlessPodmanSocket()
	patientReaper()
	tclog.SetDefault(ginkgoLogger{})

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     superuser,
				"POSTGRES_PASSWORD": password,
				"POSTGRES_DB":       superuser,
			},
			// Spec databases are disposable, so trade durability for speed.
			Cmd: []string{"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"},
			// Readiness is judged by the server answering over TCP, in
			// awaitConnections, not by the container log: podman's journald
			// log driver intermittently drops everything the server writes
			// after the entrypoint hands over to it, so a wait on the "ready
			// to accept connections" line stalls against a database that is
			// up. The port alone is not enough, since the temporary server
			// initdb runs first does not listen on TCP and the real one is
			// not yet up when it stops; the port is only the cheap first
			// gate before the query loop.
			WaitingFor: wait.ForListeningPort("5432/tcp").WithStartupTimeout(startTimeout),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	p := &Postgres{container: container}

	host, err := container.Host(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("container host: %w", err), p.Stop(ctx))
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("container port: %w", err), p.Stop(ctx))
	}
	p.adminURL = adminURL(host, port.Port())

	if err := p.awaitConnections(ctx); err != nil {
		return nil, errors.Join(err, p.Stop(ctx))
	}
	if err := p.createTemplate(ctx, migrate); err != nil {
		return nil, errors.Join(err, p.Stop(ctx))
	}
	return p, nil
}

// Attach connects to a server another process started. The URL is the one
// [Postgres.URL] returned there.
func Attach(ctx context.Context, adminURL string) (*Postgres, error) {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("attach to postgres: %w", err)
	}
	defer conn.Close(ctx) //nolint:errcheck // probe connection; nothing to flush
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("attach to postgres: %w", err)
	}
	return &Postgres{adminURL: adminURL}, nil
}

// URL is the superuser connection URL of the maintenance database. It is
// what a SynchronizedBeforeSuite passes from the process that called [Start]
// to the ones that call [Attach].
func (p *Postgres) URL() string { return p.adminURL }

// Stop removes the container. Only the process that started it has one to
// remove; elsewhere Stop is a no-op.
func (p *Postgres) Stop(ctx context.Context) error {
	if p == nil || p.container == nil {
		return nil
	}
	if err := p.container.Terminate(ctx); err != nil {
		return fmt.Errorf("stop postgres container: %w", err)
	}
	return nil
}

// Database hands the running spec its own database, cloned from the migrated
// template, and drops it when the spec ends. The spec must carry [DBLabel]:
// that is what lets `make test-unit` leave the container alone.
func (p *Postgres) Database(ctx context.Context) *pgxpool.Pool {
	ginkgo.GinkgoHelper()
	report := ginkgo.CurrentSpecReport()
	gomega.Expect(report.Labels()).To(gomega.ContainElement(DBLabel),
		"spec %q uses PostgreSQL and must be labelled %q", report.FullText(), DBLabel)
	gomega.Expect(p).NotTo(gomega.BeNil(), "PostgreSQL was not started for this suite")

	name := "spec_" + randomSuffix()
	pool, err := p.clone(ctx, name)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(func(ctx context.Context) error {
		pool.Close()
		return p.drop(ctx, name)
	})
	return pool
}

// Blank hands the running spec its own empty database and returns its
// connection URL, dropping it when the spec ends. It is what a fixture that
// brings its own migrations needs — the dagger service in
// [github.com/ivy/mill/internal/testsupport.Dagger] — where [Postgres.Database]
// hands out a clone of mill's own schema.
func (p *Postgres) Blank(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	report := ginkgo.CurrentSpecReport()
	gomega.Expect(report.Labels()).To(gomega.ContainElement(DBLabel),
		"spec %q uses PostgreSQL and must be labelled %q", report.FullText(), DBLabel)
	return p.blank(ctx)
}

// blank is [Postgres.Blank] without the label check, for a suite's own
// setup node, which carries no labels and has already chosen to start the
// container. The database is dropped by the cleanup of whichever node
// called it.
func (p *Postgres) blank(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	gomega.Expect(p).NotTo(gomega.BeNil(), "PostgreSQL was not started for this suite")

	name := "blank_" + randomSuffix()
	gomega.Expect(p.admin(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize())).To(gomega.Succeed())
	ginkgo.DeferCleanup(func(ctx context.Context) error { return p.drop(ctx, name) })
	return p.databaseURL(name)
}

// clone creates name from the template and opens a pool on it.
func (p *Postgres) clone(ctx context.Context, name string) (*pgxpool.Pool, error) {
	if err := p.admin(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()+` TEMPLATE `+templateName); err != nil {
		return nil, fmt.Errorf("create database %s: %w", name, err)
	}
	pool, err := pgxpool.New(ctx, p.databaseURL(name))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open pool on %s: %w", name, err), p.drop(ctx, name))
	}
	return pool, nil
}

func (p *Postgres) drop(ctx context.Context, name string) error {
	if err := p.admin(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// awaitConnections retries until the server answers a query over TCP. This
// is the readiness check: the temporary server initdb starts listens on a
// unix socket only, so a query that succeeds through the published port can
// only have reached the real server. Refused connections, resets from the
// port forwarder before anything listens, and "the database system is
// starting up" are all retried until the start timeout.
func (p *Postgres) awaitConnections(ctx context.Context) error {
	var err error
	for {
		if err = p.admin(ctx, `SELECT 1`); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres never accepted connections: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// createTemplate builds the database every spec clones. Cloning is a file
// copy, far cheaper than re-running migrations per spec.
func (p *Postgres) createTemplate(ctx context.Context, migrate Migrate) error {
	if err := p.admin(ctx, `CREATE DATABASE `+templateName); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	if migrate != nil {
		if err := migrate(ctx, p.databaseURL(templateName)); err != nil {
			return fmt.Errorf("migrate template database: %w", err)
		}
	}
	// Refuse ordinary connections so nothing can dirty the template, and
	// mark it so CREATE DATABASE accepts it from any role.
	if err := p.admin(ctx, `UPDATE pg_database SET datistemplate = true, datallowconn = false WHERE datname = '`+templateName+`'`); err != nil {
		return fmt.Errorf("seal template database: %w", err)
	}
	return nil
}

// admin runs one statement on the maintenance database over a fresh
// connection. CREATE and DROP DATABASE cannot run inside a transaction or
// through a pool that might be mid-transaction, so each gets its own session.
func (p *Postgres) admin(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, p.adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	_, err = conn.Exec(ctx, sql)
	return err
}

func (p *Postgres) databaseURL(name string) string {
	u, err := url.Parse(p.adminURL)
	if err != nil {
		panic(err) // adminURL was built by adminURL() and always parses
	}
	u.Path = "/" + name
	return u.String()
}

func adminURL(host, port string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(superuser, password),
		Host:     host + ":" + port,
		Path:     "/" + superuser,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// ginkgoLogger routes testcontainers' own output to the spec's writer, where
// Ginkgo shows it only for failures or under -v.
type ginkgoLogger struct{}

func (ginkgoLogger) Printf(format string, v ...any) {
	_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "testcontainers: "+format+"\n", v...)
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// useRootlessPodmanSocket points testcontainers at the rootless podman
// socket when nothing else is configured. testcontainers looks for Docker
// sockets only, and podman is the runtime this project assumes. Ryuk, the
// sidecar that removes leftovers after a crashed run, can only reach that
// socket from a privileged container, so on a host it defaults to one too.
//
// Inside a machine — where mill's own db tier runs once mill builds mill
// (docs/sandbox.md §Self-hosting) — Ryuk is turned off instead. A
// privileged container cannot start there at all: it mounts a fresh rw
// sysfs, which an unprivileged user namespace may only do when a fully
// visible sysfs is already in its mount namespace, and a machine's /sys is
// a tmpfs of read-only subtree binds; crun refuses it with "mount sysfs to
// sys: Operation not permitted" and every db suite dies in its setup. An
// unprivileged one does start — the socket there is the session user's own
// — but it is a container per parallel process to wait on at the first
// suite's cold start, for a job the machine already does: the leftovers
// Ryuk exists to remove sit on an ephemeral root that is discarded whole
// when the run ends, and that is the backstop.
func useRootlessPodmanSocket() {
	if os.Getenv("DOCKER_HOST") != "" {
		return
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	sock := filepath.Join(runtimeDir, "podman", "podman.sock")
	if _, err := os.Stat(sock); err != nil {
		return
	}
	setDefaultEnv("DOCKER_HOST", "unix://"+sock)
	if InsideMachine() {
		setDefaultEnv("TESTCONTAINERS_RYUK_DISABLED", "true")
		return
	}
	setDefaultEnv("TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED", "true")
}

// patientReaper widens the window Ryuk waits before removing the containers
// of a run it thinks has ended.
//
// Ryuk reaps ten seconds after the last connection from the test process
// closes, which is fine for a suite whose specs are all short and fatal for
// one whose specs are not: the run loop's specs hold a lease across a step
// that sleeps past a TTL, and a database removed underneath them makes every
// later query hang rather than fail. The safety net is kept — a crashed run
// still has its containers removed — it is just given a window longer than
// any spec here.
func patientReaper() {
	setDefaultEnv("RYUK_RECONNECTION_TIMEOUT", "5m")
}

func setDefaultEnv(key, value string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, value) //nolint:errcheck // fails only on an invalid key
	}
}
