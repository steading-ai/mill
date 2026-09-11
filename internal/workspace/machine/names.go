package machine

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The names a run's machine goes by on the host (docs/sandbox.md §Naming).
const (
	// MachinePrefix is what every mill machine's name begins with:
	// `mill-<id>`.
	MachinePrefix = "mill-"
	// UnitTemplate is the root-owned template a machine is an instance of:
	// `mill-machine@<id>.service`.
	UnitTemplate = "mill-machine@"
	// SocketName is the workspace socket's file name, `ws.sock`, on the
	// host under `/run/mill/<id>` and in the machine under `/run/mill`.
	SocketName = "ws.sock"
)

// idPattern is what the helper accepts as a unit instance: the machine's
// id is the run id lowered, because a ULID's alphabet is upper case and the
// helper's is not. Base32 is case-insensitive, so nothing is lost.
var idPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// ErrRunID is a run id that cannot name a machine.
var ErrRunID = errors.New("machine: run id cannot name a machine")

// IDFor is the machine id of a run: its id in lower case, refused when the
// result is not one the helper's unit instance pattern admits or is one of
// the helper's own verbs, which its dispatch would read as a verb.
func IDFor(runID string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(runID))
	if id == "" || !idPattern.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not ^[a-z0-9-]+$ once lowered", ErrRunID, runID)
	}
	switch id {
	case driver.HelperExec, driver.HelperStop, driver.HelperTerminate, driver.HelperShutdown:
		return "", fmt.Errorf("%w: %q is a verb of the mill-machine helper", ErrRunID, id)
	}
	return id, nil
}

// MachineName is `mill-<id>`: the reconciliation key.
func MachineName(id string) string { return MachinePrefix + id }

// UnitName is `mill-machine@<id>.service`.
func UnitName(id string) string { return UnitTemplate + id + ".service" }
