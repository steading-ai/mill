// Package workspace is what a step is given instead of a path and a keychain.
//
// A step never receives a filesystem path it may treat as the host and never
// a credential in its environment. It receives a unix socket and a step
// token, and everything it does to the world goes through that socket
// (docs/architecture.md §"The workspace", docs/domain-model.md §Workspace).
// This package holds the vocabulary — the [Workspace] interface, the
// capability set, the step token, the errors a refused verb carries — and the
// [Spawner] every process goes out through.
//
// [Spawner.Exec] is the single chokepoint. Cwd, uid and environment are set
// there and nowhere else, which is why the forbidigo rule in .golangci.yml
// lets os/exec be imported here and by no other package: the guarantee is
// checkable rather than aspirational.
//
// The parts:
//
//   - workspace — this package: the interface and its types
//   - workspace/local — the MVP implementation: a clone per run under the
//     mill's account, on a branch derived from the node
//   - workspace/machine — the backend of docs/sandbox.md: a machine per
//     run, reached through a driver over the host's systemd and btrfs
//     commands. Being built.
//   - workspace/socket — the verbs over a unix socket, gated by the token
//   - workspace/redact — the log redactor the socket writes through
//   - workspace/signing — the SSH key every commit is signed with
//
// The machine and remote workspaces implement [Workspace] with a real
// isolation boundary. Nothing in a workflow definition changes across them;
// if it has to, the seam leaked. Which one a worker uses is configuration
// (docs/architecture.md §"The workspace").
package workspace
