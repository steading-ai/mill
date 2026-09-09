package workspace

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/forge"
)

// Kind is which implementation is behind a workspace. The interface does not
// change across them; docs/architecture.md §"The workspace" is the table.
type Kind string

const (
	// KindLocal is a clone under the mill's own account. The MVP.
	KindLocal Kind = "local"
	// KindContainer is a clone in a rootless container. Not built.
	KindContainer Kind = "container"
	// KindRemote is a clone on another host. Not built.
	KindRemote Kind = "remote"
)

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
	// Capabilities is the effective set, from the current step's token.
	Capabilities []Capability
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
