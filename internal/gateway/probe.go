package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/ivy/mill/internal/workspace"
)

// ProbeModel is the model `mill doctor`'s request names. Counting tokens
// bills nothing, so the model only has to exist.
const ProbeModel = "claude-haiku-4-5"

// probeRun is the run id the probe's step is installed under. No run has
// it: run ids are ULIDs.
const probeRun = "doctor"

// Probe makes one cheap request through the gateway — a token count, which
// the upstream answers without billing — under a step token minted for the
// probe alone, and reports whether the upstream answered it. It is what
// `mill doctor` asks: that the credential is accepted, the upstream is
// reachable, and the path a session's request takes is whole.
func (g *Gateway) Probe(ctx context.Context) error {
	if g.credential.Empty() {
		return ErrNoCredential
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("gateway: mint a probe token: %w", err)
	}
	bearer := "probe-" + hex.EncodeToString(raw[:])
	g.Rotate(probeRun, Step{StepRunID: probeRun, StepName: "doctor", Token: workspace.TokenFor(bearer)})
	defer g.Revoke(probeRun)

	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ok"}]}`, ProbeModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, countTokensPath, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// This is every header mill itself sets on a real session's request too:
	// a session gets no more. Headers like anthropic-beta and user-agent are
	// not here because they are not mill's to set — the Claude Agent SDK
	// inside the sidecar process chooses them at request time
	// (internal/harness/claude carries no such constant today), so this
	// probe cannot mirror them without hardcoding a guess that would drift,
	// unnoticed, the moment the SDK changes what it sends. A future
	// mill-owned toggle that changes the SDK's request belongs here too.
	rec := &recorder{header: http.Header{}}
	g.ServeHTTP(rec, req)
	if rec.status != http.StatusOK {
		return fmt.Errorf("gateway: %s answered %d to the probe: %s", g.upstream.Host, rec.status, firstLine(rec.body.String()))
	}
	return nil
}

// recorder is the probe's response writer: the status and the body, held.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}

// firstLine is a body trimmed to one line for an error message.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return line
}
