package workspace

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/forge"
)

// Kind is which implementation is behind a workspace. The interface does not
// change across them; docs/architecture.md §"The workspace" is the table.
// Which one a worker uses is configuration: `--workspace` on `serve`, `work`
// and `run`, or `workspace` in the file, read by [ParseKind].
type Kind string

const (
	// KindLocal is a clone under the mill's own account. The MVP, and the
	// default.
	KindLocal Kind = "local"
	// KindMachine is a clone on the host bind-mounted into a systemd-nspawn
	// machine booted per run from an ephemeral snapshot of an image
	// subvolume, registered with systemd-machined under the run's name
	// (docs/sandbox.md §Terms). Being built: internal/workspace/machine.
	KindMachine Kind = "machine"
	// KindRemote is a clone on another host. Not built.
	KindRemote Kind = "remote"
)

// ParseKind reads a kind as configuration spells it. Empty is the default,
// local. A kind the tree names but has not built is refused by name, so an
// operator who asked for it is told that rather than shown a run that fails
// later for a reason that looks like something else.
func ParseKind(name string) (Kind, error) {
	switch Kind(strings.TrimSpace(name)) {
	case "", KindLocal:
		return KindLocal, nil
	case KindMachine:
		return KindMachine, nil
	case KindRemote:
		return "", fmt.Errorf("workspace: the %s workspace is not built; choose %s or %s", KindRemote, KindLocal, KindMachine)
	default:
		return "", fmt.Errorf("workspace: %q is not a workspace kind: %s or %s", name, KindLocal, KindMachine)
	}
}

// Capability is one thing a step may do through its workspace. The strings
// are the ledger's own text for millv1.Capability, so a capability reads the
// same in a row, on the socket, and in an error a session is shown.
type Capability string

const (
	// Read grants reading files in the checkout.
	Read Capability = "read"
	// Write grants writing files, staging, and committing on the run's branch.
	Write Capability = "write"
	// Exec grants running commands in the checkout.
	Exec Capability = "exec"
	// Network grants outbound network from commands.
	Network Capability = "network"
	// Forge grants a forge credential scoped to the project's repository.
	Forge Capability = "forge"
	// Deploy grants whatever the project's deploy step needs.
	Deploy Capability = "deploy"
)

// CapabilityFromProto maps the enum the store issues a token with onto the
// name the socket and its errors use.
func CapabilityFromProto(capability millv1.Capability) Capability {
	switch capability {
	case millv1.Capability_CAPABILITY_READ:
		return Read
	case millv1.Capability_CAPABILITY_WRITE:
		return Write
	case millv1.Capability_CAPABILITY_EXEC:
		return Exec
	case millv1.Capability_CAPABILITY_NETWORK:
		return Network
	case millv1.Capability_CAPABILITY_FORGE:
		return Forge
	case millv1.Capability_CAPABILITY_DEPLOY:
		return Deploy
	case millv1.Capability_CAPABILITY_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

// CapabilitiesFromProto maps a token's whole capability set.
func CapabilitiesFromProto(capabilities []millv1.Capability) []Capability {
	out := make([]Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		if name := CapabilityFromProto(capability); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Token is the step token as the workspace holds it: the bearer a session
// was given, the SHA-256 the store keeps, and the capability set copied from
// the step at issue.
//
// The workspace compares a presented bearer against Hash, never against
// Value, so the check is the same one the store would make and a workspace
// that never learned the bearer can still authenticate it. Value is held
// only because [Workspace.Exec] puts it in the child's environment.
type Token struct {
	// ID is the step token's ULID, for the log. Optional.
	ID string
	// Value is the bearer itself. Empty when the workspace was given only
	// the hash, in which case Exec has no token to hand a child.
	Value string
	// Hash is the hex SHA-256 of the bearer. Derived from Value when empty.
	Hash string
	// Capabilities is what the token grants.
	Capabilities []Capability
}

// TokenFor builds a token from a bearer and its capability set, computing the
// hash the socket will authenticate against.
func TokenFor(bearer string, capabilities ...Capability) Token {
	return Token{Value: bearer, Hash: HashBearer(bearer), Capabilities: capabilities}
}

// TokenFromProto builds a token from what IssueStepToken returned. The bearer
// is present on the response and nowhere else, so this is the only place a
// workspace can be given one.
func TokenFromProto(token *millv1.StepToken) Token {
	if token == nil {
		return Token{}
	}
	return Token{
		ID:           token.GetId(),
		Value:        token.GetToken(),
		Hash:         token.GetTokenHash(),
		Capabilities: CapabilitiesFromProto(token.GetCapabilities()),
	}
}

// HashBearer is how a bearer becomes the hash the store holds: hex SHA-256.
func HashBearer(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:])
}

// Live reports whether the token can authenticate anything at all. The zero
// token is what [Workspace.Rotate] is given to revoke without a successor.
func (t Token) Live() bool { return t.fingerprint() != "" }

// Matches reports whether a presented bearer is this token's. The comparison
// is over the hashes and in constant time.
func (t Token) Matches(bearer string) bool {
	want := t.fingerprint()
	if want == "" || bearer == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(HashBearer(bearer))) == 1
}

// Grants reports whether the token carries a capability.
func (t Token) Grants(capability Capability) bool {
	for _, held := range t.Capabilities {
		if held == capability {
			return true
		}
	}
	return false
}

// fingerprint is the hash to authenticate against: the one the store gave, or
// the one the bearer implies.
func (t Token) fingerprint() string {
	if t.Hash != "" {
		return t.Hash
	}
	if t.Value != "" {
		return HashBearer(t.Value)
	}
	return ""
}

// Spec is what a workspace is made from. One per run, always.
type Spec struct {
	// RunID is the run the workspace belongs to. It names the workspace's
	// directory, so two runs can never be given the same one.
	RunID string
	// NodeRef is the dagger node the run is working, such as `mill#12`. The
	// branch is derived from it; see [BranchFor].
	NodeRef string
	// Origin is the repository to clone.
	Origin string
	// Ref is what to check out: the project's default branch for a new run,
	// the run's own branch when a worker elsewhere recreates the workspace.
	Ref string
	// Egress is how the run's processes reach anything outside the
	// workspace: a proxy for the world and a gateway for the model, or
	// neither.
	Egress Egress
	// Image is the digest of the derived image the run's machine boots from
	// (docs/sandbox.md §Images): the image subvolume is named by it. The
	// local backend has no image and ignores it.
	Image string
	// Caches are the lineages restored into the run, each bound into the
	// machine at its path (docs/sandbox.md §Caching). The local backend
	// restores nothing and ignores them.
	Caches []CacheBind
	// Nested grants the machine what a run that boots machines of its own
	// needs — the wider uid range, sudo for the agent, the host's image
	// subvolumes read-only (docs/sandbox.md §Provision, §Self-hosting). The
	// local backend ignores it.
	Nested bool
}

// CacheBind is one lineage as a run sees it: the name that identifies it on
// the host and the path it is mounted at inside the machine. The host-side
// source — the run's writable snapshot of the lineage's latest generation —
// is the backend's to derive from the run id (docs/sandbox.md §Naming), so
// a spec never names a host path.
type CacheBind struct {
	// Name is the lineage's name, such as `go-mod` or `node_modules`.
	Name string
	// Path is where the machine sees it, such as `/work/node_modules`.
	Path string
	// Global marks a lineage shared by every run on the worker rather than
	// scoped to the repository (docs/sandbox.md §"Kinds and scope").
	Global bool
}

// Egress is what the workspace decided about how a session's processes
// reach anything outside it: the world through a forward proxy, the model
// through a credential gateway (docs/sandbox.md §Network, §Credentials). The
// zero value is the local backend's answer — the host's own route and the
// account's own login — and the machine backend's is a proxy and a gateway
// on the host side of its veth. Both reach a session the same way: as
// variables the workspace sets on every process it or its harness spawns,
// past every environment floor, beside the git identity it decides.
type Egress struct {
	// Proxy is the forward proxy every outbound connection goes through,
	// named to a process as HTTPS_PROXY and HTTP_PROXY. Empty means no
	// proxy was decided and the host's own routing stands.
	Proxy string
	// NoProxy is what bypasses the proxy, as NO_PROXY lists it. Read only
	// when Proxy is set.
	NoProxy string
	// Gateway is the credential gateway the model is reached through,
	// named as ANTHROPIC_BASE_URL. A session presents the step token to it
	// as ANTHROPIC_AUTH_TOKEN, so the gateway sees the token the workspace
	// last rotated in and refuses the one before it. Empty means the model
	// is reached as the account, on the SDK's own login.
	Gateway string
}

// The variables a workspace's network decisions are named to a process by.
const (
	HTTPSProxyVar        = "HTTPS_PROXY"
	HTTPProxyVar         = "HTTP_PROXY"
	NoProxyVar           = "NO_PROXY"
	ModelGatewayVar      = "ANTHROPIC_BASE_URL"
	ModelGatewayTokenVar = "ANTHROPIC_AUTH_TOKEN"
	// ToolSearchVar is set beside the gateway because the CLI turns its
	// MCP tool search off when ANTHROPIC_BASE_URL is not a first-party
	// host, and the gateway forwards bodies unchanged so nothing is lost
	// by turning it back on (docs/spikes/nspawn-per-run-sandbox.md §E14).
	ToolSearchVar = "ENABLE_TOOL_SEARCH"
)

// Environment is the decisions as the variables a process is given, with
// token as the step token it presents to the gateway. A decision that was
// not made sets nothing: under `--env-mode inherit` that is what lets the
// host's own proxy reach a session, and under `strict` it is what leaves the
// session with the SDK's login rather than an empty bearer. A gateway brings
// [ToolSearchVar] with it, so a session behind one keeps the tool search it
// would have had against the first-party host.
func (n Egress) Environment(token string) map[string]string {
	env := map[string]string{}
	if n.Proxy != "" {
		env[HTTPSProxyVar] = n.Proxy
		env[HTTPProxyVar] = n.Proxy
		env[NoProxyVar] = n.Bypass()
	}
	if n.Gateway != "" {
		env[ModelGatewayVar] = n.Gateway
		env[ModelGatewayTokenVar] = token
		env[ToolSearchVar] = "true"
	}
	return env
}

// Bypass is NO_PROXY as a process is given it: NoProxy, and the gateway's
// host when a gateway was decided beside the proxy. The gateway is on the
// host side of the machine's own link and admitted there directly; sent
// through the proxy instead, the request reaches squid as a host-bound
// connection it refuses, and the session's first call to the model fails
// with the proxy's error page (docs/sandbox.md §Network).
func (n Egress) Bypass() string {
	if n.Proxy == "" || n.Gateway == "" {
		return n.NoProxy
	}
	host := n.Gateway
	if u, err := url.Parse(n.Gateway); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	for entry := range strings.SplitSeq(n.NoProxy, ",") {
		if strings.TrimSpace(entry) == host {
			return n.NoProxy
		}
	}
	if n.NoProxy == "" {
		return host
	}
	return n.NoProxy + "," + host
}

// Handle is the workspace as the ledger records it. docs/domain-model.md
// §Workspace is the field list.
type Handle struct {
	// RunID is the run this workspace belongs to.
	RunID string
	// Kind is which implementation made it.
	Kind Kind
	// Root is the directory holding the checkout. Never given to a session.
	Root string
	// Ref is the git ref that was checked out at creation.
	Ref string
	// Branch is the run's working branch.
	Branch string
	// Socket is the unix socket a step's session reaches the workspace
	// through. With the step token, this is the session's entire world.
	Socket string
	// McpDir is a mounted MCP server's working directory and the value a
	// literal ${MCP_DIR} in its configured args or env resolves to: beside
	// the checkout, never in it, so a playwright screenshot never lands in
	// the tree a check step diffs. Removed when the workspace is.
	McpDir string
	// Capabilities is the effective set, from the current step's token.
	Capabilities []Capability
	// Sandbox is the machine behind a `machine` workspace, as
	// docs/sandbox.md §Reconciliation has the ledger record it. Nil for a
	// workspace with no machine.
	Sandbox *Sandbox
}

// Sandbox is what the ledger records of a run's machine, so a worker that
// resumes the run can find it by name and a reconciliation pass can tell an
// orphan from a live run (docs/sandbox.md §Reconciliation).
type Sandbox struct {
	// Machine is the machine's name, `mill-<id>`: the reconciliation key.
	Machine string
	// Unit is the host unit that owns it, `mill-machine@<id>.service`.
	Unit string
	// Image is the derived digest the machine booted from.
	Image string
	// Veth is the host side of the machine's network link as the host
	// named it — truncated from `ve-mill-<id>`, so recorded rather than
	// derived.
	Veth string
	// Lineages are the cache lineages restored into the run, with the
	// generation each was restored from and, once the run has finished,
	// the generation each promoted.
	Lineages []Lineage
}

// Lineage is one cache lineage as a run's record names it: which lineage,
// the generation the run's copy started from, and the generation the run
// promoted at finish (docs/sandbox.md §Caching).
type Lineage struct {
	// Scope is `global` or `repo/<id>`.
	Scope string
	// Name is the lineage's name within its scope.
	Name string
	// Restored is the generation the copy was restored from; 0 for a copy
	// that started empty.
	Restored int
	// Promoted is the generation the run promoted, 0 for a reset; nil
	// until the run has finished, and for a copy that was discarded.
	Promoted *int
}

// String is `<scope>/<name>@<restored>`, with `->` and the promoted
// generation once there is one.
func (l Lineage) String() string {
	text := fmt.Sprintf("%s/%s@%d", l.Scope, l.Name, l.Restored)
	if l.Promoted != nil {
		text += fmt.Sprintf("->%d", *l.Promoted)
	}
	return text
}

// Promoter is a workspace whose Destroy promotes the run's cache copies
// (docs/sandbox.md §Promote), and so knows, once destroyed, what each
// lineage came to. The run loop asks it after Destroy and records the
// answer in the ledger; the local backend has no caches and does not
// implement it.
type Promoter interface {
	// Promoted is the run's lineages as Destroy left them, each with its
	// promoted generation where one was made. Nil before Destroy.
	Promoted() []Lineage
}

// Entry is one name in a directory listing.
type Entry struct {
	// Name is the entry's name, without its directory.
	Name string `json:"name"`
	// Dir reports whether the entry is a directory.
	Dir bool `json:"dir"`
	// Size is the file's size in bytes; zero for a directory.
	Size int64 `json:"size"`
}

// FileStatus is one path git reports as changed, in porcelain's two-letter
// form: the index status, then the working tree's.
type FileStatus struct {
	// Code is the two-character porcelain status, such as `??` or ` M`.
	Code string `json:"code"`
	// Path is the path relative to the checkout.
	Path string `json:"path"`
}

// Status is what `git status` says of the checkout.
type Status struct {
	// Branch is the branch the checkout is on.
	Branch string `json:"branch"`
	// Clean reports whether the tree has no changes at all, which is what
	// the `clean-tree` check after a read-only step asserts.
	Clean bool `json:"clean"`
	// Files is every path git reports, staged or not.
	Files []FileStatus `json:"files"`
}

// CommitInfo is one commit, as `git log` reports it.
type CommitInfo struct {
	// SHA is the commit's full object name.
	SHA string `json:"sha"`
	// Author is the author's name and address.
	Author string `json:"author"`
	// Date is the author date, RFC 3339.
	Date string `json:"date"`
	// Subject is the first line of the message.
	Subject string `json:"subject"`
}

// DiffRequest is what to diff.
type DiffRequest struct {
	// Staged diffs the index against HEAD rather than the working tree.
	Staged bool `json:"staged"`
	// Paths narrows the diff. Empty is the whole tree.
	Paths []string `json:"paths"`
}

// LogRequest is how much history to read.
type LogRequest struct {
	// Limit caps the number of commits. Zero means a small default.
	Limit int `json:"limit"`
	// Paths narrows the history. Empty is the whole tree.
	Paths []string `json:"paths"`
}

// CommitRequest is a commit on the run's branch, signed with the mill's key.
type CommitRequest struct {
	// Message is the commit message.
	Message string `json:"message"`
	// Paths are what to stage. Empty stages every change in the checkout.
	Paths []string `json:"paths"`
	// AllowEmpty permits a commit that changes nothing.
	AllowEmpty bool `json:"allow_empty"`
}

// Git is the git half of the workspace's verbs. A session never runs git
// itself: it asks for these, so that the branch, the identity and the
// signing key are the workspace's to decide.
type Git interface {
	// Status reports the checkout's branch and every changed path.
	Status(ctx context.Context) (Status, error)
	// Diff renders a unified diff.
	Diff(ctx context.Context, request DiffRequest) (string, error)
	// Log reads the run branch's history.
	Log(ctx context.Context, request LogRequest) ([]CommitInfo, error)
	// Commit stages and commits on the run's branch, signed with the mill's
	// key, and returns the new commit's SHA.
	Commit(ctx context.Context, request CommitRequest) (CommitInfo, error)
}

// PushRequest is `git.push`: the run's branch, onto the project's repository.
// There is no branch field, because the branch is the run's and not the
// session's to name.
type PushRequest struct {
	// Force overwrites the branch on the forge, which a rebased run needs.
	Force bool `json:"force"`
}

// PushResult is what landed on the forge.
type PushResult struct {
	// Branch is the run's branch, on the forge under the same name.
	Branch string `json:"branch"`
	// SHA is the commit the branch now points at.
	SHA string `json:"sha"`
	// Remote is the repository it was pushed to. It carries no credential.
	Remote string `json:"remote"`
}

// PRRequest is `forge.pr`: the pull request for the run's branch, opened if
// the forge has none. Asking twice returns the same one, so a resumed run
// and a retried step both land on the pull request that already exists.
type PRRequest struct {
	// Title is the pull request's title when one is opened. Empty titles
	// the pull request after the branch.
	Title string `json:"title"`
	// Description is its body.
	Description string `json:"description"`
	// Target is the branch to merge into. Empty means the repository's
	// default branch.
	Target string `json:"target"`
}

// ChecksRequest is `forge.checks`: what the forge's checks say about a
// commit.
type ChecksRequest struct {
	// SHA is the commit to read. Empty means the run branch's head.
	SHA string `json:"sha"`
}

// ChecksResult is the answer, and the commit it is an answer about.
type ChecksResult struct {
	// SHA is the commit the status belongs to.
	SHA string `json:"sha"`
	// Status is `pending`, `success` or `failed`.
	Status forge.Status `json:"status"`
}

// ForgeVerbs is the forge half of the workspace's verbs, each gated by the
// `forge` capability. The session never holds the forge credential: it asks
// for a push or a pull request, and the workspace mints a credential for the
// step, uses it, and drops it (docs/architecture.md §"Identity and
// credentials").
//
// Merging is not here. A merge is the mill's own act at the end of a run,
// not something a session asks for, so it is reached through
// [github.com/ivy/mill/internal/forge.Forge] directly and has no verb.
type ForgeVerbs interface {
	// Push puts the run's branch on the forge and returns what landed.
	Push(ctx context.Context, request PushRequest) (PushResult, error)
	// PR is the run branch's pull request, opened if there is none.
	PR(ctx context.Context, request PRRequest) (forge.PullRequest, error)
	// Checks is what the forge's checks say about a commit.
	Checks(ctx context.Context, request ChecksRequest) (ChecksResult, error)
}

// Committer is the identity a workspace's own commits carry: the name and
// address git is given for every commit made in the checkout. It is the
// mill's, never a session's, which is what makes it something the `commit`
// verifier can compare a commit against.
type Committer struct {
	// Name is the name on the commit, the mill's `--name`.
	Name string `json:"name"`
	// Email is the address it commits under, the mill's `--identity`.
	Email string `json:"email"`
}

// String renders an identity the way git does: `Name <email>`.
func (c Committer) String() string { return c.Name + " <" + c.Email + ">" }

// Workspace is what a step is given instead of a path and a keychain.
//
// Create, Attach, Destroy and Rotate belong to the worker and are never
// reachable over the socket. The rest are the verbs a session may call,
// gated by the current token's capability set.
type Workspace interface {
	// Create makes a fresh workspace for a run: a clone at the spec's ref, a
	// branch derived from the node, and a socket to reach them through.
	Create(ctx context.Context, spec Spec) (*Handle, error)
	// Attach reopens the workspace a previous worker left behind, for a run
	// resumed on the same host.
	Attach(ctx context.Context, spec Spec) (*Handle, error)
	// Destroy stops the socket and removes the checkout.
	Destroy(ctx context.Context) error
	// Rotate installs the step token the socket will authenticate against
	// and hand to children. The token it replaces stops working at once; the
	// zero token revokes without a successor.
	Rotate(ctx context.Context, token Token) error
	// Exec runs a command inside the checkout. This is the single chokepoint
	// for every process a step spawns: cwd, uid and environment are set here
	// and nowhere else.
	Exec(ctx context.Context, command Command) (*Process, error)
	// Read reads a file in the checkout.
	Read(ctx context.Context, path string) ([]byte, error)
	// Write writes a file in the checkout, inside the tree only.
	Write(ctx context.Context, path string, content []byte) error
	// List lists a directory in the checkout.
	List(ctx context.Context, path string) ([]Entry, error)
	// Git is the git verbs.
	Git() Git
	// Forge is the forge verbs.
	Forge() ForgeVerbs
	// Redact registers values the workspace must scrub from every log line
	// it writes from here on.
	Redact(values ...string)
}

// Sink is where a verb's own activity comes from: it started, what it wrote
// to each stream once the whole of it was in hand, and how it ended
// (docs/activity.md's verb_started/verb_output/verb). Unlike a check's Sink
// (internal/check.Sink), this is not incremental: a git command's output is
// whole by the time [internal/workspace/verbs.Host.Run] has anything to
// report.
//
// It lives here rather than in internal/workspace/verbs, which is where it
// is called from, because [ActivitySink] — the capability a backend
// implements with it — is part of the [Workspace] contract this package
// owns, and verbs already imports this package: a Sink declared over there
// and referenced from here would be an import cycle.
type Sink interface {
	// Started is called once, before the verb's command spawns.
	Started(verb string, args map[string]string)
	// Output is called once per non-empty stream, with the whole buffer.
	Output(verb, stream string, data []byte)
	// Ended is called once the command has exited or failed to run, with the
	// error Run itself returns — nil on success.
	Ended(verb string, err error)
}

// ActivitySink is a workspace whose verbs can record onto an attempt's
// activity. The run loop calls SetActivitySink when it opens an attempt's
// writer and clears it the same way when the attempt closes, so a verb
// called between attempts — there shouldn't be one, but nothing enforces
// that structurally — writes nothing rather than into a closed file. A
// workspace with no verbs to record does not implement it, and the loop
// asks nothing of it.
type ActivitySink interface {
	// SetActivitySink installs sink, or clears it with nil.
	SetActivitySink(sink Sink)
}

// TwinSource is a workspace that can pull a session's harness transcript out
// for the twin (docs/activity.md's ".twin.tar.zst", the seal's second file
// beside the frame). It is not part of [Workspace]: a check step's
// workspace, or recovery with no live workspace open, has nothing to offer
// and is simply not asked, matching [ActivitySink]'s optional-interface
// pattern.
type TwinSource interface {
	// CaptureTwin tars every file of sessionID's transcript — including any
	// subagent's, whose own session id is not known in advance — and
	// returns it uncompressed; the caller applies its own outer compression.
	// A session with no transcript on disk returns no error and a reader
	// with no files: a missing twin is logged and does not fail a seal.
	CaptureTwin(ctx context.Context, sessionID string) (io.Reader, error)
}

// CacheCandidate is what a workspace that carries cache lineages implements
// beside [Workspace]: the run loop calls Candidate each time a step closes
// on evidence, before the step's slice is stopped, so the run's writable
// cache copies are snapshotted as the promotion candidate at that moment
// and a later evidence-closed step replaces it (docs/sandbox.md §"Step
// boundary", §Promote). A cancelled, timed-out or failed step never reaches
// it. A workspace without lineages — the local backend — does not
// implement it, and the loop asks nothing of it.
type CacheCandidate interface {
	// Candidate snapshots the run's cache copies as the candidate of the
	// named step.
	Candidate(ctx context.Context, step string) error
}

// StepBoundary is what a workspace whose steps run inside a reapable unit
// implements beside [Workspace]: the run loop calls BeginStep before a step
// runs and EndStep after it, so every process the step spawned — daemons and
// containers included — is reaped when the step ends and nothing outlives it
// (docs/sandbox.md §"Step boundary"). The machine backend runs each step
// under its own systemd slice, `mill-step-<ordinal>.slice`, and reaps the
// step by stopping the slice; the local backend has no such boundary and
// does not implement it, so the loop asks nothing of it.
//
// EndStep carries whether the step closed on evidence, because a step that
// did leaves a promotion candidate — a snapshot of the run's cache copies
// taken before the slice stops — and one that was cancelled, timed out or
// failed leaves none (docs/sandbox.md §Promote). The candidate is therefore
// this interface's when a workspace implements it: a workspace that carries
// both [CacheCandidate] and StepBoundary takes the candidate in EndStep, and
// the loop leaves its CacheCandidate hook alone.
type StepBoundary interface {
	// BeginStep names the slice the step's execs will run under; the next
	// step's slice is not begun until the previous one reports inactive,
	// which EndStep waits for.
	BeginStep(ctx context.Context, ordinal int) error
	// EndStep stops the step's slice, reaping everything it spawned, after
	// taking the promotion candidate when the step closed on evidence. It
	// waits for the slice to report inactive before it returns, so the next
	// BeginStep may proceed.
	EndStep(ctx context.Context, ordinal int, closedOnEvidence bool) error
}

// StepResumer is a [StepBoundary] that remembers which step slice was in
// flight when a previous worker left the run: on attach, that slice is
// stopped — whatever was running in it is not trusted to still be the step
// — and the next BeginStep recreates it under the same ordinal, so the run
// resumes at the ledger's step in a fresh slice of the same name
// (docs/sandbox.md §Reconciliation). The run loop asks it once, after the
// workspace is opened, for the ordinal its count continues from.
type StepResumer interface {
	// NextOrdinal is the ordinal the next BeginStep should carry: 1 for a
	// workspace no step has begun in, and the reaped slice's own ordinal
	// for one attached mid-step.
	NextOrdinal() int
}

// Provisioned is a workspace that provisions a machine for its run
// (docs/sandbox.md §Provision), and so has two things the local backend
// does not. Provisioning leaves findings for the run's first session — an
// invalid `mill.toml`, a derived build that fell back to base — and the
// machine is what a workflow's preparatory steps ready (docs/sandbox.md
// §"Preparatory steps"). The run loop delivers the findings and runs the
// preparatory steps only in a workspace that is one: on the operator's own
// host there is no machine to ready, and a step written for one — `mise
// trust /work` — would act on the host's own configuration instead.
type Provisioned interface {
	// Findings are what provisioning left for the first session, in the
	// order they were found, each one line in the words the session is
	// given.
	Findings() []string
}

// Install is one report from the `dnf` shim: the packages a session put
// into its machine's ephemeral root, which the next run's image will not
// have (docs/sandbox.md §"The dnf shim").
type Install struct {
	// Packages as the shim named them.
	Packages []string
}

// Reporter is a workspace whose socket answers `report.install`: the shim's
// report is taken over the socket and held until the run loop drains it
// into the run's provenance, because the workspace has no ledger of its own
// to write. A workspace that is not one refuses the verb, and the shim
// tolerates that.
type Reporter interface {
	// ReportInstall takes one report. An empty package list is refused.
	ReportInstall(ctx context.Context, packages []string) error
	// Installs hands over every report taken since the last call, oldest
	// first, and forgets them.
	Installs() []Install
}

// ErrLostSandbox is a run whose sandbox is gone: the ledger says the run
// is live and this host once made a machine for it, but the machine is no
// longer registered. The run fails with this as evidence and nothing boots
// a fresh machine for it, because the workspace's state was the machine's
// and is not the ledger's to reconstruct (docs/sandbox.md §Reconciliation).
var ErrLostSandbox = errors.New("workspace: lost sandbox")

// BranchFor is the run's working branch, derived from the node ref: `mill/`
// and the ref with everything git forbids in a branch name replaced. A node
// ref of `mill#12` becomes `mill/mill-12`. A node ref with nothing usable in
// it yields no branch, and the workspace stays on whatever it cloned.
//
// Deriving rather than storing is what makes two concurrent runs on different
// nodes land on different branches without coordinating.
func BranchFor(nodeRef string) string {
	var b strings.Builder
	previousDash := false
	for _, r := range nodeRef {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
			previousDash = false
		default:
			// One dash for any run of characters a branch name may not
			// carry, so `mill#12` and `mill #12` do not collide by
			// accident of spacing.
			if !previousDash {
				b.WriteByte('-')
				previousDash = true
			}
		}
	}
	name := strings.TrimRight(b.String(), "-.")
	if name == "" {
		return ""
	}
	return "mill/" + name
}

// ErrNotImplemented is what a verb this workspace does not implement returns.
// A session that asks for one is told so, rather than being told the verb
// does not exist.
var ErrNotImplemented = errors.New("workspace: not implemented")

// ErrUnauthorized is a bearer that is not the live step token: revoked,
// rotated away, or never issued.
var ErrUnauthorized = errors.New("workspace: unauthorized")

// ErrNoForge is a forge verb on a workspace whose run has no forge: a dagger
// project with no repository, or a workspace built without one.
var ErrNoForge = errors.New("workspace: no forge is configured for this run")

// ErrOutsideTree is a path that leaves the checkout.
var ErrOutsideTree = errors.New("workspace: path is outside the checkout")

// CapabilityError is a verb refused because the step's token does not carry
// the capability the verb needs. It names the capability, because
// docs/domain-model.md §"Capability set" requires a definition error to be
// legible to the session rather than mysterious.
type CapabilityError struct {
	// Verb is what was asked for, such as `fs.write`.
	Verb string
	// Missing is the capability the token does not carry.
	Missing Capability
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("workspace: %s requires the %s capability, which this step token does not carry", e.Verb, e.Missing)
}
