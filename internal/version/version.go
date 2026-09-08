// Package version reports the build's version.
//
// The value is injected at link time by the Makefile:
//
//	-ldflags "-X github.com/ivy/mill/internal/version.version=<semver>"
//
// A build without the flag reports a development placeholder so that every
// build, including "go run", still yields a semver-shaped string.
package version

// version is set through ldflags. It must stay an unexported package-level
// string variable for -X to reach it.
var version = "0.0.0-dev"

// String returns the version of this build.
func String() string {
	return version
}
