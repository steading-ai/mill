package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The robot credential `mill image build-base --push` presents to Harbor:
// one line, `<robot account>:<secret>`, delivered by LoadCredential= into
// $CREDENTIALS_DIRECTORY and read from nowhere else (docs/sandbox.md
// §Base).
const (
	// CredentialsDirectoryEnv is systemd's variable naming the directory
	// LoadCredential= delivers credentials into.
	CredentialsDirectoryEnv = "CREDENTIALS_DIRECTORY"
	// RobotCredentialName is the file under it holding the Harbor robot
	// credential.
	RobotCredentialName = "harbor-robot"
	// HarborHostArg is the Containerfile's build argument naming the Harbor
	// host the image's registries.conf routes pulls through.
	HarborHostArg = "HARBOR_HOST"
	// DateTagLayout is the tag a base build carries beside `latest`.
	DateTagLayout = "2006-01-02"
)

// Robot is a Harbor robot account's credential.
type Robot struct {
	// User is the robot account's name, such as `robot$mill+builder`.
	User string
	// Secret is its secret. It goes down podman's stdin and nowhere else.
	Secret string
}

// DefaultRobotPath is where the credential is read from when nothing names
// a file: $CREDENTIALS_DIRECTORY/harbor-robot. Empty when systemd
// delivered nothing.
func DefaultRobotPath() string {
	dir := os.Getenv(CredentialsDirectoryEnv)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, RobotCredentialName)
}

// LoadRobot reads `<user>:<secret>` from path.
func LoadRobot(path string) (Robot, error) {
	if path == "" {
		return Robot{}, fmt.Errorf("image: no robot credential file is named; set %s or --robot-credential-file", CredentialsDirectoryEnv)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's configuration
	if err != nil {
		return Robot{}, fmt.Errorf("image: robot credential: %w", err)
	}
	user, secret, found := strings.Cut(strings.TrimSpace(string(raw)), ":")
	if !found || user == "" || secret == "" {
		return Robot{}, fmt.Errorf("image: robot credential %s is not one line of <robot>:<secret>", path)
	}
	return Robot{User: user, Secret: secret}, nil
}

// BaseBuild is one `mill image build-base`.
type BaseBuild struct {
	// Context is the directory holding the base Containerfile,
	// `images/base` in this repository.
	Context string
	// Date is the tag beside `latest`; the build's day when zero.
	Date time.Time
	// Push sends both tags to the registry with Robot.
	Push bool
	// Robot is the credential a push presents.
	Robot Robot
}

// BaseResult is what a base build produced.
type BaseResult struct {
	// Tags are the references the image carries, dated then latest.
	Tags []string
	// Digest is the image's manifest digest: what `ResolveBase` will find
	// once the tags are pushed.
	Digest Digest
	// Pushed is whether the tags went to the registry.
	Pushed bool
}

// BuildBase builds the base image from its Containerfile on the host —
// `podman build` outside any machine, through the same driver verb the
// derived build uses — tags it `<registry>/<repo>:<date>` and `:latest`,
// and pushes both when asked. The Harbor host goes in as a build argument
// so the image's registries.conf names the same proxy projects the host's
// does.
func (s *Store) BuildBase(ctx context.Context, build BaseBuild) (BaseResult, error) {
	if build.Context == "" {
		return BaseResult{}, fmt.Errorf("image: build-base needs the directory holding the base Containerfile")
	}
	date := build.Date
	if date.IsZero() {
		date = s.config.Now()
	}
	repository := s.config.Registry + "/" + s.config.Repository
	tags := []string{repository + ":" + date.Format(DateTagLayout), repository + ":" + DefaultTag}

	output, err := s.host.BuildImageOnHost(ctx, driver.ImageBuild{
		Context: build.Context,
		Tags:    tags,
		Args:    map[string]string{HarborHostArg: s.config.Registry},
	})
	if err != nil {
		return BaseResult{}, fmt.Errorf("%w\n%s", err, Tail(output))
	}
	raw, err := s.host.ImageDigest(ctx, tags[1])
	if err != nil {
		return BaseResult{}, err
	}
	digest, err := ParseDigest(raw)
	if err != nil {
		return BaseResult{}, err
	}
	result := BaseResult{Tags: tags, Digest: digest}
	if !build.Push {
		return result, nil
	}
	if build.Robot.User == "" {
		return result, fmt.Errorf("image: push needs the robot credential")
	}
	if err := s.host.Login(ctx, s.config.Registry, build.Robot.User, build.Robot.Secret); err != nil {
		return result, err
	}
	defer func() { _ = s.host.Logout(context.WithoutCancel(ctx), s.config.Registry) }()
	for _, tag := range tags {
		if err := s.host.PushImage(ctx, tag); err != nil {
			return result, err
		}
	}
	result.Pushed = true
	return result, nil
}
