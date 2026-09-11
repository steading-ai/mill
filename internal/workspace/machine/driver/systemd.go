package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// Helper is the root-owned helper everything that enters a running
	// machine goes through — exec, a stop inside it, terminate — always
	// as `sudo -n <Helper> <verb>`; [DefaultHelper]. Its verbs and what
	// each admits are homestead's mill_machine role's (docs/sandbox.md
	// §Privilege).
	Helper string
	// Btrfs snapshots and deletes subvolumes; `btrfs`.
	Btrfs string
	// Podman pulls, builds, exports and pushes images; `podman`.
	Podman string
	// Tar extracts an exported image into a subvolume; `tar`.
	Tar string
	// Setcap restores file capabilities on an exported tree; `setcap`.
	Setcap string
	// Du estimates a generation's size at promotion; `du`.
	Du string
	// DetectVirt says whether the worker is inside a machine;
	// `systemd-detect-virt`.
	DetectVirt string
	// Sudo runs the verbs that write the image directory as root, because
	// that directory is root's on the host (docs/sandbox.md §Storage): the
	// subvolume create, rename and delete, the extraction, setcap, and the
	// existence check. Empty runs them as the worker, which is right for a
	// host where the directory is the worker's and for every spec that
	// stubs the tools. The helper is the exception: it is root's by
	// construction and always runs through sudo, [HelperSudo] when this is
	// empty.
	Sudo string
	// HelperSudo is the sudo the helper runs through when Sudo is empty;
	// `sudo`. It exists so a spec can stub the one without the other.
	HelperSudo string
}

// DefaultHelper is where homestead installs the helper.
const DefaultHelper = "/usr/libexec/mill-machine"

// The helper's verbs. They cannot be run ids: the helper's dispatch reads
// its first argument as one of these and anything else as the run to boot.
const (
	HelperExec      = "exec"
	HelperStop      = "stop"
	HelperTerminate = "terminate"
	HelperShutdown  = "shutdown"
)

// HelperRefused is the status the helper exits with when it refuses a
// request on its own terms — a property outside its allow-list, a slice
// outside the step pattern, a machine that is not mill's — before anything
// runs. It is EX_CONFIG, and it is never the command's own status.
const HelperRefused = 78

// withDefaults is the commands with every empty one named.
func (c Commands) withDefaults() Commands {
	if c.Systemctl == "" {
		c.Systemctl = "systemctl"
	}
	if c.Machinectl == "" {
		c.Machinectl = "machinectl"
	}
	if c.Helper == "" {
		c.Helper = DefaultHelper
	}
	if c.HelperSudo == "" {
		c.HelperSudo = "sudo"
	}
	if c.Sudo != "" {
		c.HelperSudo = c.Sudo
	}
	if c.Btrfs == "" {
		c.Btrfs = "btrfs"
	}
	if c.Podman == "" {
		c.Podman = "podman"
	}
	if c.Tar == "" {
		c.Tar = "tar"
	}
	if c.Setcap == "" {
		c.Setcap = "setcap"
	}
	if c.Du == "" {
		c.Du = "du"
	}
	if c.DetectVirt == "" {
		c.DetectVirt = "systemd-detect-virt"
	}
	return c
}

// privileged is a command line run as root when Sudo is named: `sudo -n
// <path> <args>`, never prompting, so a host that has not granted the
// worker the command fails at once rather than waiting on a terminal.
func (c Commands) privileged(path string, args ...string) (string, []string) {
	if c.Sudo == "" {
		return path, args
	}
	return c.Sudo, append([]string{"-n", path}, args...)
}

// helper is one helper verb as it is run: `sudo -n <helper> <verb> <args>`.
func (c Commands) helper(verb string, args ...string) (string, []string) {
	return c.HelperSudo, append([]string{"-n", c.Helper, verb}, args...)
}

// systemdDriver is the host reached through its own tools — systemctl,
// machinectl, the mill-machine helper and btrfs — every one of them spawned
// through [workspace.Spawner.Exec] and nothing else. Mill drives systemd
// through its command line rather than D-Bus (docs/adrs/018): the commands
// are what the specification names, what the spike ran, and what an
// operator can rerun by hand from a log line.
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

// StopUnit is `systemctl stop <unit>` on the host, or the helper's `stop
// <machine> <unit>` when a machine is named.
func (d *systemdDriver) StopUnit(ctx context.Context, machine, unit string) error {
	if machine == "" {
		_, err := d.run(ctx, d.commands.Systemctl, "stop", unit)
		return err
	}
	path, args := d.commands.helper(HelperStop, machine, unit)
	_, err := d.run(ctx, path, args...)
	return err
}

// Terminate is the helper's `terminate <machine>`.
func (d *systemdDriver) Terminate(ctx context.Context, machine string) error {
	path, args := d.commands.helper(HelperTerminate, machine)
	_, err := d.run(ctx, path, args...)
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

// Exec is the helper's `exec <machine>`, with the unit, the slice, the
// directory, the user and the environment as the properties the helper
// admits; the helper adds `--pipe --wait --quiet --collect`, `Delegate=yes`
// and `KillMode=mixed` itself. The process comes back with its pipes still
// open: `--pipe` connects them to the service through the helper, and
// streaming them is the caller's.
//
// The environment goes as `-p Environment=NAME=VALUE`, one per variable,
// which is the form the helper's allow-list matches.
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
	args := []string{request.Machine}
	if request.Unit != "" {
		args = append(args, "--unit="+request.Unit)
	}
	args = append(args, "-p", "Slice="+request.Slice)
	if request.Dir != "" {
		args = append(args, "-p", "WorkingDirectory="+request.Dir)
	}
	if request.User != "" {
		args = append(args, "-p", "User="+request.User)
	}
	for _, name := range slices.Sorted(maps.Keys(request.Env)) {
		args = append(args, "-p", "Environment="+name+"="+request.Env[name])
	}
	args = append(args, "--")
	args = append(args, request.Argv...)
	path, args := d.commands.helper(HelperExec, args...)
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: path, Args: args})
	if err != nil {
		return nil, fmt.Errorf("machine: exec in %s: %w", request.Machine, err)
	}
	return process, nil
}

// MachineInterfaces is `machinectl status -n0 <name>`, read for its
// `Iface:` line, which names the host-side interfaces machined knows of.
// `machinectl show` carries the same fact only as interface indexes it
// prints as unprintable.
func (d *systemdDriver) MachineInterfaces(ctx context.Context, name string) ([]string, error) {
	out, err := d.run(ctx, d.commands.Machinectl, "status", "--no-pager", "-n", "0", name)
	if err != nil {
		if strings.Contains(err.Error(), "No machine") {
			return nil, fmt.Errorf("%w: %s", ErrNoMachine, name)
		}
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && key == "Iface" {
			return strings.Fields(value), nil
		}
	}
	return nil, nil
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

// DeleteSubvolume is `btrfs subvolume delete <path>`, as root when the
// commands name sudo, then `btrfs filesystem sync` of the parent directory
// when asked, because the parent is on the filesystem the subvolume was on
// and still exists once it is gone.
func (d *systemdDriver) DeleteSubvolume(ctx context.Context, path string, sync bool) error {
	if err := d.runPrivileged(ctx, d.commands.Btrfs, "subvolume", "delete", path); err != nil {
		return err
	}
	if !sync {
		return nil
	}
	return d.runPrivileged(ctx, d.commands.Btrfs, "filesystem", "sync", filepath.Dir(path))
}

// SetReadOnly is `btrfs property set -ts <path> ro true|false`, as the
// worker: the subvolumes it is asked of are the worker's own.
func (d *systemdDriver) SetReadOnly(ctx context.Context, path string, readOnly bool) error {
	_, err := d.run(ctx, d.commands.Btrfs, "property", "set", "-ts", path, "ro", strconv.FormatBool(readOnly))
	return err
}

// DiskUsage is `du -sk <path>`, read as bytes.
func (d *systemdDriver) DiskUsage(ctx context.Context, path string) (uint64, error) {
	out, err := d.run(ctx, d.commands.Du, "-sk", path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, fmt.Errorf("machine: du -sk %s said nothing", path)
	}
	kib, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("machine: du -sk %s: %q is not a size", path, fields[0])
	}
	return kib * 1024, nil
}

// Exists is a stat as the worker, or `test -e` as root when the commands
// name sudo, because a directory the worker may not read is one it cannot
// stat into.
func (d *systemdDriver) Exists(ctx context.Context, path string) (bool, error) {
	if d.commands.Sudo == "" {
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("machine: stat %s: %w", path, err)
	}
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: d.commands.Sudo, Args: []string{"-n", "test", "-e", path}})
	if err != nil {
		return false, fmt.Errorf("machine: test -e %s: %w", path, err)
	}
	_, stderr, result := drain(process)
	switch result.Status {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("machine: test -e %s exited %d: %s", path, result.Status, strings.TrimSpace(stderr))
	}
}

// CreateSubvolume is `btrfs subvolume create <path>`, as root when the
// commands name sudo.
func (d *systemdDriver) CreateSubvolume(ctx context.Context, path string) error {
	return d.runPrivileged(ctx, d.commands.Btrfs, "subvolume", "create", path)
}

// Rename is rename(2) as the worker, or `mv -T` as root when the commands
// name sudo. Both are one atomic rename within a filesystem.
func (d *systemdDriver) Rename(ctx context.Context, oldPath, newPath string) error {
	if d.commands.Sudo == "" {
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("machine: rename %s to %s: %w", oldPath, newPath, err)
		}
		return nil
	}
	return d.runPrivileged(ctx, "mv", "-T", oldPath, newPath)
}

// Setcap is `setcap <capability> <path>`, as root when the commands name
// sudo.
func (d *systemdDriver) Setcap(ctx context.Context, capability, path string) error {
	return d.runPrivileged(ctx, d.commands.Setcap, capability, path)
}

// PullImage is `podman pull --quiet <reference>`.
func (d *systemdDriver) PullImage(ctx context.Context, reference string) error {
	_, err := d.run(ctx, d.commands.Podman, "pull", "--quiet", reference)
	return err
}

// ImageDigest is `podman image inspect --format {{.Digest}}`: the manifest
// digest the registry knows the image by, which for a built image is what
// a push will record.
func (d *systemdDriver) ImageDigest(ctx context.Context, reference string) (string, error) {
	out, err := d.run(ctx, d.commands.Podman, "image", "inspect", "--format", "{{.Digest}}", reference)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(out)
	if digest == "" {
		return "", fmt.Errorf("machine: %s has no digest", reference)
	}
	return digest, nil
}

// BuildImageOnHost is `podman build` on the host, outside any machine. It
// is the one container the worker runs there (docs/sandbox.md §"Derived
// images"), and the only driver verb whose output is returned whole: a
// failed build's last lines are what the run is told.
func (d *systemdDriver) BuildImageOnHost(ctx context.Context, build ImageBuild) (string, error) {
	if build.Context == "" {
		return "", errors.New("machine: build with no context directory")
	}
	if len(build.Tags) == 0 {
		return "", errors.New("machine: build with no tag")
	}
	args := []string{"build"}
	for _, tag := range build.Tags {
		args = append(args, "--tag", tag)
	}
	if build.Containerfile != "" {
		args = append(args, "--file", build.Containerfile)
	}
	for _, name := range slices.Sorted(maps.Keys(build.Args)) {
		args = append(args, "--build-arg", name+"="+build.Args[name])
	}
	args = append(args, build.Context)
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: d.commands.Podman, Args: args})
	if err != nil {
		return "", fmt.Errorf("machine: podman build: %w", err)
	}
	stop := killOnCancel(ctx, process)
	_ = process.Stdin.Close()
	output, err := combined(process)
	result := process.Wait()
	stop()
	if err != nil {
		return output, fmt.Errorf("machine: podman build: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return output, fmt.Errorf("machine: podman build: %w", ctxErr)
	}
	if result.Status != 0 {
		return output, fmt.Errorf("machine: podman build exited %d", result.Status)
	}
	return output, nil
}

// ExportImage is `podman create <image>`, then `podman export` piped into
// `tar -x` in dir — as root when the commands name sudo, so the tree's
// owners are the image's and not the worker — then `podman rm`. The
// container is removed on every path out.
func (d *systemdDriver) ExportImage(ctx context.Context, image, dir string) error {
	out, err := d.run(ctx, d.commands.Podman, "create", image)
	if err != nil {
		return err
	}
	container := strings.TrimSpace(out)
	defer func() {
		_, _ = d.run(context.WithoutCancel(ctx), d.commands.Podman, "rm", "--force", container)
	}()

	export, err := d.spawner.Exec(ctx, workspace.Command{Path: d.commands.Podman, Args: []string{"export", container}})
	if err != nil {
		return fmt.Errorf("machine: podman export: %w", err)
	}
	stopExport := killOnCancel(ctx, export)
	_ = export.Stdin.Close()
	tarPath, tarArgs := d.commands.privileged(d.commands.Tar, "--extract", "--directory", dir, "--file", "-")
	extract, err := d.spawner.Exec(ctx, workspace.Command{Path: tarPath, Args: tarArgs})
	if err != nil {
		stopExport()
		_ = export.Signal(syscall.SIGKILL)
		_, _, _ = drain(export)
		return fmt.Errorf("machine: tar: %w", err)
	}
	stopExtract := killOnCancel(ctx, extract)

	var exportErr, extractErr []byte
	var copyErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		exportErr, _ = io.ReadAll(export.Stderr)
	}()
	go func() {
		_, copyErr = io.Copy(extract.Stdin, export.Stdout)
		_ = extract.Stdin.Close()
	}()
	extractOut, _ := io.ReadAll(extract.Stdout)
	extractErr, _ = io.ReadAll(extract.Stderr)
	extractResult := extract.Wait()
	<-done
	exportResult := export.Wait()
	stopExport()
	stopExtract()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("machine: export %s: %w", image, err)
	}
	if exportResult.Status != 0 {
		return fmt.Errorf("machine: podman export exited %d: %s", exportResult.Status, strings.TrimSpace(string(exportErr)))
	}
	if extractResult.Status != 0 {
		return fmt.Errorf("machine: tar exited %d: %s%s", extractResult.Status, strings.TrimSpace(string(extractOut)), strings.TrimSpace(string(extractErr)))
	}
	if copyErr != nil {
		return fmt.Errorf("machine: export %s: %w", image, copyErr)
	}
	return nil
}

// RemoveImage is `podman rmi <image>`.
func (d *systemdDriver) RemoveImage(ctx context.Context, image string) error {
	_, err := d.run(ctx, d.commands.Podman, "rmi", image)
	return err
}

// Login is `podman login --username <user> --password-stdin <registry>`;
// the secret goes down the pipe and appears in no argument list.
func (d *systemdDriver) Login(ctx context.Context, registry, user, secret string) error {
	process, err := d.spawner.Exec(ctx, workspace.Command{
		Path: d.commands.Podman,
		Args: []string{"login", "--username", user, "--password-stdin", registry},
	})
	if err != nil {
		return fmt.Errorf("machine: podman login %s: %w", registry, err)
	}
	stop := killOnCancel(ctx, process)
	_, _ = io.WriteString(process.Stdin, secret)
	_ = process.Stdin.Close()
	_, stderr, result := drain(process)
	stop()
	if result.Status != 0 {
		return fmt.Errorf("machine: podman login %s exited %d: %s", registry, result.Status, strings.TrimSpace(stderr))
	}
	return nil
}

// Logout is `podman logout <registry>`.
func (d *systemdDriver) Logout(ctx context.Context, registry string) error {
	_, err := d.run(ctx, d.commands.Podman, "logout", registry)
	return err
}

// PushImage is `podman push <reference>`.
func (d *systemdDriver) PushImage(ctx context.Context, reference string) error {
	_, err := d.run(ctx, d.commands.Podman, "push", reference)
	return err
}

// Statfs is the statfs(2) system call: not a command, but the same truth
// `btrfs filesystem usage` would report and the one eviction reads.
// Container is `systemd-detect-virt --container`: the manager's name on
// stdout, or `none` and a non-zero exit outside any container, which is
// not an error.
func (d *systemdDriver) Container(ctx context.Context) (string, error) {
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: d.commands.DetectVirt, Args: []string{"--container"}})
	if err != nil {
		return "", fmt.Errorf("machine: %s --container: %w", d.commands.DetectVirt, err)
	}
	_ = process.Stdin.Close()
	stdout, _, _ := drain(process)
	name := strings.TrimSpace(stdout)
	if name == "none" {
		return "", nil
	}
	return name, nil
}

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
// method but Exec — which hands the process to its caller instead — and
// the pipelines above goes through here. A context that ends kills the
// command's process group: the spawner starts processes and does not stop
// them, so a budget is enforced here or nowhere.
func (d *systemdDriver) run(ctx context.Context, path string, args ...string) (string, error) {
	process, err := d.spawner.Exec(ctx, workspace.Command{Path: path, Args: args})
	if err != nil {
		return "", fmt.Errorf("machine: %s %s: %w", path, strings.Join(args, " "), err)
	}
	stop := killOnCancel(ctx, process)
	_ = process.Stdin.Close()
	stdout, stderr, result := drain(process)
	stop()
	if err := ctx.Err(); err != nil {
		return stdout, fmt.Errorf("machine: %s %s: %w", path, strings.Join(args, " "), err)
	}
	if result.Status != 0 {
		return stdout, fmt.Errorf("machine: %s %s exited %d: %s",
			path, strings.Join(args, " "), result.Status, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// runPrivileged is run through sudo when the commands name it.
func (d *systemdDriver) runPrivileged(ctx context.Context, path string, args ...string) error {
	path, args = d.commands.privileged(path, args...)
	_, err := d.run(ctx, path, args...)
	return err
}

// drain reads a process's stdout and stderr to their ends and reaps it.
func drain(process *workspace.Process) (stdout, stderr string, result workspace.Result) {
	var errOut []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		errOut, _ = io.ReadAll(process.Stderr)
	}()
	out, _ := io.ReadAll(process.Stdout)
	<-done
	return string(out), string(errOut), process.Wait()
}

// combined reads a process's stdout and stderr as they arrive into one
// transcript, in the order the reads land. The caller reaps the process.
func combined(process *workspace.Process) (string, error) {
	var mu sync.Mutex
	var buf strings.Builder
	var errs []error
	var wg sync.WaitGroup
	for _, reader := range []io.Reader{process.Stdout, process.Stderr} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunk := make([]byte, 4096)
			for {
				n, err := reader.Read(chunk)
				if n > 0 {
					mu.Lock()
					buf.Write(chunk[:n])
					mu.Unlock()
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	return buf.String(), errors.Join(errs...)
}

// killOnCancel ends the process's group when ctx ends first. The returned
// function stands the watch down once the process has been reaped.
func killOnCancel(ctx context.Context, process *workspace.Process) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = process.Signal(syscall.SIGKILL)
		case <-done:
		}
	}()
	return func() { close(done) }
}
