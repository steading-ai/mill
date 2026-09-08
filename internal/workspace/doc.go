// Package workspace is the single chokepoint for spawning processes.
//
// Every checkout, git invocation, agent session and check that mill runs on
// behalf of a step goes through here, so that sandboxing, credentials and
// cancellation have one place to live. The forbidigo rule in .golangci.yml
// enforces it: os/exec may be imported by this package and nothing else.
//
// The implementation arrives with the workspace node; this file exists so
// the chokepoint the lint rule names is a real package from the start.
package workspace
