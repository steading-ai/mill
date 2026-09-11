package socket

import "github.com/ivy/mill/internal/workspace"

// The request and response bodies of every verb. JSON over HTTP, one POST to
// /v1/<verb> per call, the bearer in the Authorization header.

// PathRequest is what fs.read and fs.list take: one path relative to the
// checkout.
type PathRequest struct {
	Path string `json:"path"`
}

// ReadResponse carries a file's bytes. JSON renders them base64, so a binary
// file survives the wire.
type ReadResponse struct {
	Content []byte `json:"content"`
}

// WriteRequest is fs.write: a path inside the tree and its new content.
type WriteRequest struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// WriteResponse confirms what was written.
type WriteResponse struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

// ListResponse carries a directory's entries.
type ListResponse struct {
	Entries []workspace.Entry `json:"entries"`
}

// ExecRequest is one command to run in the checkout. There is no environment
// field: the environment is the workspace's to set, and a step naming its own
// would be the hole this whole seam exists to close.
type ExecRequest struct {
	Path  string   `json:"path"`
	Args  []string `json:"args"`
	Dir   string   `json:"dir"`
	Stdin string   `json:"stdin"`
}

// ExecResponse is how the command ended and what it produced.
type ExecResponse struct {
	Status int32  `json:"status"`
	Signal int32  `json:"signal"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// DiffResponse carries a unified diff.
type DiffResponse struct {
	Diff string `json:"diff"`
}

// LogResponse carries the run branch's history.
type LogResponse struct {
	Commits []workspace.CommitInfo `json:"commits"`
}

// RedactRequest registers values the workspace must scrub from its logs.
type RedactRequest struct {
	Values []string `json:"values"`
}

// RedactResponse confirms how many values were taken.
type RedactResponse struct {
	Registered int `json:"registered"`
}

// ReportInstallRequest is report.install: the packages the `dnf` shim just
// installed into the machine, as it named them.
type ReportInstallRequest struct {
	Packages []string `json:"packages"`
}

// ReportInstallResponse confirms how many packages were recorded.
type ReportInstallResponse struct {
	Packages int `json:"packages"`
}
