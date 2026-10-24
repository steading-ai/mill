package testsupport

import (
	"os"
	"strings"
)

// ScratchEnv is the environment for a subprocess that operates on a
// repository of the spec's own rather than the one the spec process runs
// from: this process's environment with every GIT_* variable removed, then
// extra.
//
// Under a git hook the process inherits GIT_DIR, GIT_INDEX_FILE,
// GIT_WORK_TREE and the rest of git's GIT_* variables, and every git a
// scratch subprocess runs — directly, or through make or a hook — would
// otherwise act on the hook's repository regardless of its working
// directory. Every spec that spawns git, or anything that spawns git, builds
// that environment here.
func ScratchEnv(extra ...string) []string {
	env := os.Environ()
	clean := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			clean = append(clean, kv)
		}
	}
	return append(clean, extra...)
}

// SpecGit is what a spec's own fixture git adds over [MinimalEnv]: none of
// the operator's configuration, an identity of the spec's own, and a fixed
// locale, so what git prints is what the spec matches.
func SpecGit(extra ...string) []string {
	return append([]string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_AUTHOR_NAME=spec",
		"GIT_AUTHOR_EMAIL=spec@example.test",
		"GIT_COMMITTER_NAME=spec",
		"GIT_COMMITTER_EMAIL=spec@example.test",
		"LC_ALL=C",
	}, extra...)
}

// MinimalEnv is the environment for a subprocess that needs nothing of the
// host but where its programs are and whose home it is: PATH and HOME from
// this process, then extra. No GIT_* variable reaches it unless extra names
// one, so it is as safe under a git hook as [ScratchEnv], and tighter.
func MinimalEnv(extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}, extra...)
}
