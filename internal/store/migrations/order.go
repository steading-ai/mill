package migrations

import "fmt"

// Violation is one migration this branch adds that collides with the
// target's numbering: a file the target does not have, claiming a version
// at or below the target's highest. Merged as-is, it lands out of order.
type Violation struct {
	File          string
	Version       int64
	TargetHighest int64
}

// Error names the file, its number, and the next free number to renumber it
// to.
func (v Violation) Error() string {
	return fmt.Sprintf("%s claims version %d, at or below the target's highest (%d); renumber it to at least %d",
		v.File, v.Version, v.TargetHighest, v.TargetHighest+1)
}

// CheckOrder reports a Violation for every migration in ours that target
// does not already have and whose version is at or below the highest
// version target carries.
func CheckOrder(target, ours map[string]int64) []Violation {
	var highest int64
	for _, version := range target {
		if version > highest {
			highest = version
		}
	}
	var violations []Violation
	for name, version := range ours {
		if _, ok := target[name]; ok {
			continue
		}
		if version <= highest {
			violations = append(violations, Violation{File: name, Version: version, TargetHighest: highest})
		}
	}
	return violations
}
