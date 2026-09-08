// Package workspace is the single chokepoint for spawning processes.
//
// Every checkout, git invocation, agent session and check that mill runs on
// behalf of a step goes through here, so that sandboxing, credentials and
// cancellation have one place to live. The forbidigo rule in .golangci.yml
// enforces it: os/exec may be imported by this package and nothing else.
//
// Only the spawning half is here: Local.Exec starts a process in a process
// group of its own, with the environment the caller's step is allowed, so
// the harness adapter has a chokepoint to go through. The checkout, git, the
// socket and its verbs arrive with the workspace node.
package workspace
