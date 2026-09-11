package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"syscall"

	"github.com/ivy/mill/internal/workspace"
)

// Commands are the host tools the driver runs. Each is a path, or empty for
// the tool by name on the mill's own PATH — the spawner resolves a bare name
// there, not on its floor, so a spec that stubs a tool names the stub by
// path.
type Commands struct {
	// Systemctl starts and stops units; `systemctl`.
	Systemctl string
	// Machinectl shows and lists machines; `machinectl`.
	Machinectl string
	// SystemdRun runs a transient unit inside a machine; `systemd-run`.
	SystemdRun string
	// Btrfs snapshots and deletes subvolumes; `btrfs`.
	Btrfs string
}

// withDefaults is the commands with every empty one named.
func (c Commands) withDefaults() Commands {
	if c.Systemctl == "" {
		c.Systemctl = "systemctl"
	}
	if c.Machinectl == "" {
		c.Machinectl = "machinectl"
	}
	if c.SystemdRun == "" {
		c.SystemdRun = "systemd-run"
	}
	if c.Btrfs == "" {
		c.Btrfs = "btrfs"
	}
	return c
}

// systemdDriver is the host reached through its own tools — systemctl,
// machinectl, systemd-run and btrfs — every one of them spawned through
// [workspace.Spawner.Exec] and nothing else. Mill drives systemd through its
// command line rather than D-Bus (docs/adrs/018): the commands are what the
// specification names, what the spike ran, and what an operator can rerun
// by hand from a log line.
//
// What the spike found about privilege — `systemctl start` and `stop` of the
// template unit work from the operator's uid under polkit, and `systemd-run
// --machine` does not (docs/spikes/nspawn-per-run-sandbox.md, E0) — lands
// here when the exec node builds on it: the caller asks for an exec, and
// this driver is where a helper would be named instead.
type systemdDriver struct {
	spawner  workspace.Spawner
	commands Commands
}

// NewSystemd is the driver for a real host. The spawner is how every command
// is run and its floor is the environment they see; commands names them.
func NewSystemd(spawner workspace.Spawner, commands Commands) Driver {
	return &systemdDriver{spawner: spawner, commands: commands.withDefaults()}
}

// StartUnit is `systemctl start <unit>`.
func (d *systemdDriver) StartUnit(ctx context.Context, unit string) error {
	_, err := d.run(ctx, d.commands.Systemctl, "start", unit)
	return err
}

// StopUnit is `systemctl stop <unit>`, addressed into the machine when one
// is named.
func (d *systemdDriver) StopUnit(ctx context.Context, machine, unit string) error {
	args := []string{}
	if machine != "" {
		args = append(args, "--machine="+machine)
	}
	args = append(args, "stop", unit)
	_, err := d.run(ctx, d.commands.Systemctl, args...)
	return err
}

// ShowMachine is `machinectl show <name>`, read as the `Key=Value` lines it
// prints.
func (d *systemdDriver) ShowMachine(ctx context.Context, name string) (Machine, error) {
	out, err := d.run(ctx, d.commands.Machinectl, "show", name,
		"--property=Name", "--property=State", "--property=Class", "--property=Service")
	if err != nil {
		if strings.Contains(err.Error(), "No machine") {
			return Machine{}, fmt.Errorf("%w: %s", ErrNoMachine, name)
		}
		return Machine{}, err
	}
	var machine Machine
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "Name":
			machine.Name = value
		case "State":
			machine.State = value
		case "Class":
			machine.Class = value
		case "Service":
			machine.Service = value
		}
	}
	return machine, nil
}

// Exec is `systemd-run --machine=<name> --pipe --wait --quiet --collect`,
// with the unit, the slice, the directory, the user and the environment as
// properties. The process comes back with its pipes still open: `--pipe`
// connects them to the service, and streaming them is the caller's.
//
// The environment goes as `--setenv=NAME=VALUE`, which sets the same
// `Environment=` property `-p` would and takes the value verbatim, where a
// property assignment is parsed for quotes first.
func (d *systemdDriver) Exec(ctx context.Context, request ExecRequest) (*workspace.Process, error) {
	if request.Machine == "" {
		return nil, errors.New("machine: exec names no machine")
	}
	if request.Slice == "" {
		return nil, errors.New("machine: exec under no step slice; every process runs under one (docs/sandbox.md §Exec)")
	}
	if len(request.Argv) == 0 {
		return nil, errors.New("machine: exec with nothing to run")
	}
	args := []string{
		"--machine=" + request.Machine,
		"--pipe", "--wait", "--quiet", "--collect",
		"--property=Slice=" + request.Slice,
	}
	if request.Unit != "" {
		args = append(args, "--unit="+request.Unit)
	}
	if request.Dir != "" {
		args = append(args, "--property=WorkingDirectory="+request.Dir)
	}
	if request.User != "" {
		args = append(args, "--property=User="+request.User)
	}
	for _, name := range slices.Sorted(maps.Keys(request.Env)) {
		args = append(args, "--setenv="+name+"="+request.Env[name])
	}
	args = append(args, "--")
	args = append(args, request.Argv...)
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: d.commands.SystemdRun, Args: args})
	if err != nil {
		return nil, fmt.Errorf("machine: exec in %s: %w", request.Machine, err)
	}
	return process, nil
}

// ListMachines is `machinectl list --no-legend`, one machine per line: name,
// class, service, then columns this driver does not read.
func (d *systemdDriver) ListMachines(ctx context.Context) ([]Machine, error) {
	out, err := d.run(ctx, d.commands.Machinectl, "list", "--no-legend", "--no-pager")
	if err != nil {
		return nil, err
	}
	var machines []Machine
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		machines = append(machines, Machine{Name: fields[0], Class: fields[1], Service: fields[2]})
	}
	return machines, nil
}

// Snapshot is `btrfs subvolume snapshot [-r] <source> <destination>`.
func (d *systemdDriver) Snapshot(ctx context.Context, source, destination string, readOnly bool) error {
	args := []string{"subvolume", "snapshot"}
	if readOnly {
		args = append(args, "-r")
	}
	args = append(args, source, destination)
	_, err := d.run(ctx, d.commands.Btrfs, args...)
	return err
}

// DeleteSubvolume is `btrfs subvolume delete <path>`.
func (d *systemdDriver) DeleteSubvolume(ctx context.Context, path string) error {
	_, err := d.run(ctx, d.commands.Btrfs, "subvolume", "delete", path)
	return err
}

// Statfs is the statfs(2) system call: not a command, but the same truth
// `btrfs filesystem usage` would report and the one eviction reads.
func (d *systemdDriver) Statfs(path string) (Filesystem, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Filesystem{}, fmt.Errorf("machine: statfs %s: %w", path, err)
	}
	size := uint64(stat.Bsize) //nolint:gosec // a block size is never negative
	return Filesystem{
		Type:  filesystemName(stat.Type),
		Free:  stat.Bavail * size,
		Total: stat.Blocks * size,
	}, nil
}

// filesystemName spells a superblock magic the way `stat -f` does.
func filesystemName(magic int64) string {
	switch magic {
	case 0x9123683E:
		return TypeBtrfs
	case 0x58465342:
		return "xfs"
	case 0xEF53:
		return "ext4"
	case 0x01021994:
		return "tmpfs"
	case 0x794C7630:
		return "overlay"
	case 0x2FC12FC1:
		return "zfs"
	case 0x6969:
		return "nfs"
	case 0x65735546:
		return "fuse"
	default:
		return fmt.Sprintf("%#x", magic)
	}
}

// run spawns one host command through the chokepoint, drains it, and turns a
// non-zero exit into an error carrying what the command said. Every driver
// method but Exec — which hands the process to its caller instead — goes
// through here.
func (d *systemdDriver) run(ctx context.Context, path string, args ...string) (string, error) {
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: path, Args: args})
	if err != nil {
		return "", fmt.Errorf("machine: %s %s: %w", path, strings.Join(args, " "), err)
	}
	_ = process.Stdin.Close()
	var stderr []byte
	var stderrErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		stderr, stderrErr = io.ReadAll(process.Stderr)
	}()
	stdout, stdoutErr := io.ReadAll(process.Stdout)
	<-done
	result := process.Wait()
	if err := errors.Join(stdoutErr, stderrErr); err != nil {
		return "", fmt.Errorf("machine: %s %s: %w", path, strings.Join(args, " "), err)
	}
	if result.Status != 0 {
		return string(stdout), fmt.Errorf("machine: %s %s exited %d: %s",
			path, strings.Join(args, " "), result.Status, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}
