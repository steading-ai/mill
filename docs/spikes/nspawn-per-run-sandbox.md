# Spike: a machine boots from the operator's uid through polkit, execs, runs podman, reaps a step, and shares a workspace

`docs/sandbox.md` and [ADR-018](../adrs/018-a-runs-sandbox-is-a-systemd-nspawn-machine.md)
rest on claims about `systemd-nspawn` on `core` that nobody had run. This spike
runs them: a machine started and entered from the operator's uid through polkit
(E0), booted from an ephemeral btrfs snapshot and entered with `systemd-run -M`
(E1), running rootless podman as `agent` (E2), reaped by stopping a step slice
(E3), and sharing a bind-mounted workspace at the same uid on both sides (E4).
Every verdict below is an observation of output; the commands are given as run,
and where a command was run more than once with different flags, every attempt
is here. The image the machines boot is built by `spikes/nspawn/build-image.sh`
from `spikes/nspawn/Containerfile.spike`. The four scenarios are executable in
`spikes/nspawn/spike_test.go` — the live tier of `docs/sandbox.md` §Testing —
and run with `MILL_SANDBOX_HOST=1 ginkgo ./spikes/nspawn` on a host with root,
nspawn and the image; the E0 scenario fails there at its exec assertion, which
is E0's result. Later spike nodes (network and suite, storage, nested,
gateway) append their sections to this file.

## What was under test

| | |
|---|---|
| Host | `core`, Fedora release 44 (Forty Four), bare metal |
| Kernel | `7.1.13-200.fc44.x86_64` |
| systemd | `systemd 259 (259.8-1.fc44)`; `systemd-nspawn --version` reports the same |
| podman (host, rootless) | `podman version 5.8.4` |
| SELinux | `Enforcing` |
| `/var/lib/machines` | `btrfs rw,noatime,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=257,subvol=/@machines` — root, `0700` |
| `/var/lib/mill` | `btrfs rw,noatime,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill` — owned by the operator |
| Host networking | NetworkManager; `systemd-networkd` inactive, so a machine's `--network-veth` gets no host-side address. Network is the next spike node's; this one loads images into the machine by hand where it needs one |
| Harbor | `https://harbor.tailcbcc0e.ts.net/v2/` answered `502 Bad Gateway` for the whole session, so every pull that routes through it failed; where an image was needed it was pulled on the host straight from the upstream registry (`CONTAINERS_REGISTRIES_CONF=/dev/null`) and piped in with `podman save | podman load` |
| Operator | `ivy`, uid 1000, passwordless `sudo` |
| Date | 2026-09-10 23:45 – 2026-09-11 00:15 PDT |

The preflight the work order asked for:

```
$ findmnt -no FSTYPE,OPTIONS /var/lib/machines
btrfs rw,noatime,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=257,subvol=/@machines
$ findmnt -no FSTYPE,OPTIONS /var/lib/mill
btrfs rw,noatime,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill
$ ls -ld /var/lib/machines /var/lib/mill
drwx------. 1 root root 0 Sep 10 23:06 /var/lib/machines
drwxr-x---. 1 ivy  ivy  0 Sep 10 23:08 /var/lib/mill
```

Summary of verdicts, one line each; the detail is in the sections.

| | Verdict | What decided it |
|---|---|---|
| Setup | done | `1004M` subvolume; 18.7 s build and export |
| E0 | **fail** (start and stop pass, exec fails) | `systemd-run -M` as the operator: `Failed to open namespace of PID 2111175: Permission denied`; `machinectl shell` runs the command but returns 0 for an exit status of 42. Also: a stop within ~200 ms of start hangs 90 s and orphans the machine in `mill-e0.scope` |
| E1 | pass | `State=running` in 0.08 s, bus reachable in 0.31 s, `rc=42` with `out` on stdout and `err` on stderr, snapshot `.#machine.mill-image-spike…` in `btrfs subvolume list`, no AVCs |
| E2 | pass, after three fixes | `overlay systemd` and hello as `agent`, once `newuidmap` had its file capability back, `agent`'s subordinate range fit inside the machine's 65536 uids, and `/proc` was unmasked inside the machine; no AVCs |
| E3 | pass with `--cgroups=split` | without it the container lands under `user@1000.service` and survives the slice stop; with it and `Delegate=yes` on the exec unit everything sits under the step slice and is dead in the next step |
| E4 | pass with `:idmap` on the bind | `--private-users-ownership=auto` idmaps the root tree only; `/work` was uid 65534 inside until the bind carried `:idmap`, then the same SHA both sides and owner `1000` on the host |

---

## Setup — the base image as a btrfs subvolume

`spikes/nspawn/Containerfile.spike` is what the work order lists: `fedora:44`,
`systemd dbus-broker podman git sudo curl`, the native-extension toolchain, mise
at `/usr/local/bin/mise`, `agent` at uid 1000, `podman.socket` enabled for the
system and for `agent`'s user instance (linger file plus a `sockets.target.wants`
link, since `loginctl` and `systemctl --user` do not run in a build), the
`dnf install` sudoers line, `storage.conf` with `driver = "overlay"`, the Harbor
mapping, `mise trust /work`, and an empty `/etc/machine-id` so the machine gets
one at boot. Two things differ from the order, both found by running it:

- Harbor's proxy-cache projects are `dockerhub-cache` and `quay-cache`, not
  `dockerhub` and `quay` — `ansible/roles/podman/templates/registries.conf.j2`
  in homestead and `/etc/containers/registries.conf` on `core` both say so.
- `agent`'s subordinate uid and gid ranges are `10000:55536`, not `useradd`'s
  default `524288:65536`; E2 shows why.

`spikes/nspawn/build-image.sh` is the work order's three commands plus two
things found by running them: a `--signature-policy` for the build, and
`setcap` on `newuidmap` and `newgidmap` after the export.

### First attempt: the host's policy rejects the base image

```
$ ./spikes/nspawn/build-image.sh
STEP 1/10: FROM registry.fedoraproject.org/fedora:44
Trying to pull registry.fedoraproject.org/fedora:44...
Error: creating build container: unable to copy from source docker://registry.fedoraproject.org/fedora:44: copying system image from manifest list: Source image rejected: Running image docker://registry.fedoraproject.org/fedora:44 is rejected by policy.
./spikes/nspawn/build-image.sh  0.04s user 0.03s system 8% cpu 0.830 total
```

`/etc/containers/policy.json` on `core` defaults to `reject` and lists the
registries homestead's podman role allows; `registry.fedoraproject.org` is not
among them. `spikes/nspawn/policy-spike.json` admits that one registry and
nothing else, and the script passes it with `--signature-policy`. (For the
real pipeline, `mill#59`, this is a homestead finding: the base image's
registry needs a policy entry or a Harbor proxy project.)

### Second attempt: build and export, timed

```
$ ( time ./spikes/nspawn/build-image.sh )
STEP 1/10: FROM registry.fedoraproject.org/fedora:44
Trying to pull registry.fedoraproject.org/fedora:44...
Getting image source signatures
Copying blob sha256:c1925c3a4b59e3a62a6e0017135008ff21b7f349da07dbdb3f42b309c34f8716
Copying config sha256:74d8893a22f206df7a713e9c39101040e7b1788102a810018f84a7982987dd3b
Writing manifest to image destination
STEP 2/10: RUN dnf -y install       systemd dbus-broker podman git sudo curl       gcc gcc-c++ make pkgconf python3 openssl-devel zlib-devel libffi-devel       libyaml-devel readline-devel ncurses-devel gdbm-devel sqlite-devel     && dnf clean all
Updating and loading repositories:
 Fedora 44 - x86_64 - Updates           100% |  10.4 MiB/s |  11.4 MiB |  00m01s
 Fedora 44 openh264 (From Cisco) - x86_ 100% |   4.5 KiB/s |   5.3 KiB |  00m01s
 Fedora 44 - x86_64                     100% |  27.0 MiB/s |  40.4 MiB |  00m02s
Repositories loaded.
[… 195 packages installed; lines 13–714 of the log elided; the reviewer can re-run the script …]
Removed 18 files, 11 directories (total of 101 MiB). 0 errors occurred.
--> 6ffbc65915d0
STEP 3/10: RUN curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh     && /usr/local/bin/mise --version
mise: installing mise...

#####                                                                      7.6%
#################################                                         46.0%
############################################################              84.4%
######################################################################## 100.0%
mise: installed successfully to /usr/local/bin/mise
mise: run the following to activate mise in your shell:
echo "eval \"\$(/usr/local/bin/mise activate bash)\"" >> ~/.bashrc

mise: run `mise doctor` to verify this is set up correctly
2026.9.5 linux-x64 (2026-09-10)
--> 2c14c6db3633
STEP 4/10: RUN useradd -u 1000 -m agent     && mkdir -p /work && chown agent:agent /work
--> 4242f4fa1b81
STEP 5/10: RUN systemctl enable podman.socket     && mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/agent     && mkdir -p /home/agent/.config/systemd/user/sockets.target.wants     && ln -s /usr/lib/systemd/user/podman.socket          /home/agent/.config/systemd/user/sockets.target.wants/podman.socket     && chown -R agent:agent /home/agent/.config
Created symlink '/etc/systemd/system/sockets.target.wants/podman.socket' → '/usr/lib/systemd/system/podman.socket'.
--> 23fd84b709bd
STEP 6/10: RUN echo 'agent ALL=(root) NOPASSWD: /usr/bin/dnf install *' > /etc/sudoers.d/agent-dnf     && chmod 0440 /etc/sudoers.d/agent-dnf
--> 8285680ca51f
STEP 7/10: RUN printf '[storage]\ndriver = "overlay"\nrunroot = "/run/containers/storage"\ngraphroot = "/var/lib/containers/storage"\n'       > /etc/containers/storage.conf
--> a0ce071686b3
STEP 8/10: RUN mkdir -p /etc/containers/registries.conf.d     && printf '%s\n'       '[[registry]]'       'prefix = "docker.io"'       'location = "harbor.tailcbcc0e.ts.net/dockerhub-cache"'       ''       '[[registry]]'       'prefix = "quay.io"'       'location = "harbor.tailcbcc0e.ts.net/quay-cache"'       > /etc/containers/registries.conf.d/harbor.conf
--> 433499a64ee2
STEP 9/10: RUN su agent -c '/usr/local/bin/mise trust /work'     && su agent -c '/usr/local/bin/mise trust --show'
mise trusted /work
mise No trusted config files found.
--> c5cc71cf9b4f
STEP 10/10: RUN : > /etc/machine-id
COMMIT mill-base-spike
--> 2c0e7b406f6a
Successfully tagged localhost/mill-base-spike:latest
2c0e7b406f6a8753729f194572da55df400ecd70de98769d16865a991499478f
Create subvolume '/var/lib/machines/mill-image-spike'
a7ea4bda6aa24a79f40712de4fd1f31b1c87c2b6af9d94f8f8f18d10534adf2a
./spikes/nspawn/build-image.sh  11.41s user 4.70s system 86% cpu 18.687 total
```

The `mise trust --show` line reads "No trusted config files found" because
`/work` is empty at build time; `~agent/.local/state/mise/trusted-configs/`
holds `work-7d24b00784adc381`, so the trust is recorded.

```
$ sudo du -sh /var/lib/machines/mill-image-spike
1004M	/var/lib/machines/mill-image-spike
$ podman images mill-base-spike
REPOSITORY                 TAG         IMAGE ID      CREATED        SIZE
localhost/mill-base-spike  latest      2c0e7b406f6a  9 seconds ago  1.03 GB
$ sudo btrfs subvolume list /var/lib/machines
ID 256 gen 16 top level 5 path @mill
ID 257 gen 10 top level 5 path @machines
ID 265 gen 17 top level 257 path mill-image-spike
```

**Image size: 1004 MiB on the subvolume (1.03 GB as an OCI image). Build plus
export wall clock: 18.687 s** (`11.41s user 4.70s system 86% cpu`), of which
the `dnf install` of 195 packages is nearly all; the rebuild at the end of the
session, with every layer but the changed ones cached, took 2.262 s.

Two facts about the exported tree that the experiments below run into:

```
$ sudo ls -Zd /var/lib/machines /var/lib/machines/mill-image-spike/ /var/lib/machines/mill-image-spike/usr/bin/podman /var/lib/machines/mill-image-spike/home/agent
system_u:object_r:systemd_machined_var_lib_t:s0 /var/lib/machines
                                              ? /var/lib/machines/mill-image-spike/
               system_u:object_r:unlabeled_t:s0 /var/lib/machines/mill-image-spike/usr/bin/podman
               system_u:object_r:unlabeled_t:s0 /var/lib/machines/mill-image-spike/home/agent
$ matchpathcon /var/lib/machines/mill-image-spike/usr/bin/podman
/var/lib/machines/mill-image-spike/usr/bin/podman	system_u:object_r:systemd_machined_var_lib_t:s0
$ sudo ls -ldn /var/lib/machines/mill-image-spike/usr/bin/podman /var/lib/machines/mill-image-spike/home/agent
drwx------. 1 1000 1000       90 Sep 10 23:49 /var/lib/machines/mill-image-spike/home/agent
-rwxr-xr-x. 1    0    0 45220848 Jun 25 17:00 /var/lib/machines/mill-image-spike/usr/bin/podman
```

The tree carries an explicit `unlabeled_t` label on every file (`tar` wrote the
`security.selinux` xattr the OCI layer carried), and the uids are the image's
own — root is 0 and `agent` is 1000 on the host — which is what
`--private-users-ownership=auto` then maps. No experiment below needed the
labels changed; see §SELinux at the end.

### The rebuild from the committed script

After E2 the fixes were put into the Containerfile and the script, the
subvolume deleted, and the script run again, so what is committed is what
produces a working image:

```
$ sudo btrfs subvolume delete /var/lib/machines/mill-image-spike && ( time ./spikes/nspawn/build-image.sh )
Delete subvolume 265 (no-commit): '/var/lib/machines/mill-image-spike'
STEP 10/10: RUN : > /etc/machine-id
COMMIT mill-base-spike
--> 1aae82590a69
Successfully tagged localhost/mill-base-spike:latest
1aae82590a69cfbefba5d84e9f240eaf0332d15803beb08fbca1df84ae927759
Create subvolume '/var/lib/machines/mill-image-spike'
1757fe86bd2be670c104f2b3d5eabb63081b0fc5b390df6a9aab8e5870e460cf
./spikes/nspawn/build-image.sh  1.31s user 2.02s system 147% cpu 2.262 total
$ sudo getcap /var/lib/machines/mill-image-spike/usr/bin/newuidmap /var/lib/machines/mill-image-spike/usr/bin/newgidmap
/var/lib/machines/mill-image-spike/usr/bin/newuidmap cap_setuid=ep
/var/lib/machines/mill-image-spike/usr/bin/newgidmap cap_setgid=ep
$ sudo cat /var/lib/machines/mill-image-spike/etc/subuid
agent:10000:55536
$ sudo du -sh /var/lib/machines/mill-image-spike
1004M	/var/lib/machines/mill-image-spike
```

And a boot from it, with E2's `/proc` recipe, to show the committed artefacts
produce a machine podman runs in:

```
# the rebuilt image, from the committed script: boot, unmask, podman as agent
$ /tmp/spike-out/reboot-e1.sh
Running as unit: mill-run-e1.service; invocation ID: 951332fa140d420bb676fb37e9b1d889
running
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- mount -t proc -o nosuid,nodev,noexec proc /proc
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman info --format '{{.Store.GraphDriverName}} {{.Host.CgroupManager}}'
overlay systemd
$ CONTAINERS_REGISTRIES_CONF=/dev/null podman pull -q quay.io/podman/hello && podman save quay.io/podman/hello | sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman load | tail -1
5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Getting image source signatures
Copying blob sha256:2114fc8b70586b9325dde6fd26066d9951414dcdfb3995f41d51d1995cf3bd9d
Copying config sha256:5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Writing manifest to image destination
Loaded image: quay.io/podman/hello:latest
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello | head -1; echo "rc=${pipestatus[1]}"
!... Hello Podman World ...!
rc=0
$ sudo systemctl stop mill-run-e1; machinectl list --no-pager
No machines.
```

**Verdict: the image builds and exports in under twenty seconds to a 1004 MiB subvolume; the committed script produces a machine rootless podman runs in.**

---

## E0 — Machines from the operator's uid through polkit

The template unit and the polkit rule, installed as the work order gives them:

```
$ sudo tee /etc/systemd/system/mill-machine@.service <<'UNIT'
[Service]
Type=notify
NotifyAccess=all
KillMode=mixed
ExecStart=/usr/bin/systemd-nspawn --boot --quiet --machine=%i --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false
UNIT
$ sudo tee /etc/polkit-1/rules.d/50-mill-machine-spike.rules <<'RULES'
polkit.addRule(function(action, subject) {
  if (subject.user != "ivy") return polkit.Result.NOT_HANDLED;
  if (action.id == "org.freedesktop.systemd1.manage-units" &&
      /^mill-machine@.*\.service$/.test(action.lookup("unit"))) return polkit.Result.YES;
  if (action.id == "org.freedesktop.machine1.shell" ||
      action.id == "org.freedesktop.machine1.manage-machines") return polkit.Result.YES;
  return polkit.Result.NOT_HANDLED;
});
RULES
$ sudo systemctl daemon-reload; echo "daemon-reload rc=$?"
daemon-reload rc=0
```

Everything that follows in this section ran **as `ivy`, without `sudo`**.

### Start and exec

```
$ systemctl start mill-machine@mill-e0.service; echo "start rc=$?"
start rc=0
$ machinectl show mill-e0 -p State
State=running
$ systemd-run -M mill-e0 --pipe --wait --quiet --collect -- sh -c 'echo out; exit 42'; echo "exec rc=$?"
Failed to connect to system scope bus via machine transport: Permission denied
exec rc=1
```

Start succeeded through the polkit rule and the machine booted (`State=running`
three seconds later). `systemd-run -M` was refused. With debug logging, the
refusal is not polkit's — `sd-bus`'s machine transport opens
`/proc/<leader>/ns/*` itself and enters the namespaces, which needs
`CAP_SYS_ADMIN`; machined and its polkit actions are never consulted:

```
$ SYSTEMD_LOG_LEVEL=debug systemd-run -M mill-e0 --pipe --wait --quiet --collect -- sh -c 'echo out; exit 42' 2>&1 | tail -5
Bus n/a: changing state UNSET → OPENING
sd-bus: connecting bus to machine mill-e0...
Failed to open namespace of PID 2111175: Permission denied
Bus n/a: changing state OPENING → CLOSED
Failed to connect to system scope bus via machine transport: Permission denied
```

### The fallback primitive: `machinectl shell`

`machinectl shell` goes through machined's `OpenMachineShell`, which the rule's
`org.freedesktop.machine1.shell` admits:

```
$ machinectl shell mill-e0 /bin/sh -c 'echo out; exit 42'; echo "shell rc=$?"
Connected to machine mill-e0. Press ^] three times within 1s to exit session.
out
Connection to machine mill-e0 terminated.
shell rc=0
$ machinectl shell mill-e0 /bin/sh -c 'echo out; echo err >&2; exit 42' </dev/null | cat; echo "shell-piped rc=${pipestatus[1]}"
Connected to machine mill-e0. Press ^] three times within 1s to exit session.
out
err
Connection to machine mill-e0 terminated.
shell-piped rc=0
```

The command runs, its output arrives (with the connection banners, and stderr
merged into stdout because the channel is a pty), and **the exit status does not
propagate: `machinectl shell` returns 0 for a command that exited 42**, whether
or not its stdin is a terminal. machined's varlink `io.systemd.Machine.Open`
(`varlinkctl introspect /run/systemd/machine/io.systemd.Machine`) is the same
primitive — it returns `ptyFileDescriptor` and `ptyPath` and nothing else — so
there is no polkit-mediated exec on systemd 259 that carries an exit status or
separates stderr.

### Other transports tried

`sd-bus` accepts `user@machine`, which the manual says uses `machinectl shell`
underneath; in 259 it spawns `systemd-run -Mmachine … systemd-stdio-bridge`,
so it needs the very thing that was refused:

```
$ systemd-run -M root@mill-e0 --pipe --wait --quiet --collect -- sh -c 'echo out; echo err >&2; exit 42'; echo "exec rc=$?"
Failed to connect to system scope bus via machine transport: Permission denied
Failed to start transient service unit: Transport endpoint is not connected
exec rc=1
$ systemd-run -M agent@mill-e0 --pipe --wait --quiet --collect -- sh -c 'echo out; echo err >&2; exit 42'; echo "exec rc=$?"
Failed to connect to system scope bus via machine transport: Permission denied
Failed to start transient service unit: Transport endpoint is not connected
exec rc=1
$ systemd-run -M mill-e0:/ --pipe --wait --quiet --collect -- sh -c 'echo out; echo err >&2; exit 42'; echo "exec rc=$?"
Invalid --machine= specified: mill-e0:/
exec rc=1
```

`systemctl -M`, which the step boundary (`systemctl -M … stop mill-step-<n>.slice`)
needs, uses the same transport and fails the same way. Then stop, and the
refusals the rule must produce:

```
$ systemctl -M mill-e0 list-units --type=slice --no-pager --no-legend; echo "systemctl -M rc=$?"
Failed to connect to system scope bus via machine transport: Permission denied
systemctl -M rc=1
$ machinectl status mill-e0 --no-pager | head -8
mill-e0(05d3a7c2ef0a4290a681f79517d9e189)
	   Since: Thu 2026-09-10 23:50:01 PDT; 1min 14s ago
	  Leader: 2111175 (systemd)
	 Superv.: 2111170 (systemd-nspawn)
	 Service: systemd-nspawn; class container
	    Root: /var/lib/machines/.#machine.mill-image-spike98aacb0f08fe815b
	   Iface: ve-mill-e0
	      OS: Fedora Linux 44 (Container Image)
$ systemctl stop mill-machine@mill-e0.service; echo "stop rc=$?"
stop rc=0
$ machinectl list --no-pager
No machines.
$ systemctl start mill-run-anything-else.service; echo "other rc=$? (must be refused)"
Failed to start mill-run-anything-else.service: Access denied as the requested operation requires interactive authentication. However, interactive authentication has not been enabled by the calling program.
See system logs and 'systemctl status mill-run-anything-else.service' for details.
other rc=1 (must be refused)
$ systemctl start systemd-networkd.service; echo "existing-unit rc=$? (must be refused)"
Failed to start systemd-networkd.service: Access denied as the requested operation requires interactive authentication. However, interactive authentication has not been enabled by the calling program.
See system logs and 'systemctl status systemd-networkd.service' for details.
existing-unit rc=1 (must be refused)
```

### Found by the executable spec: a stop in the first 200 ms orphans the machine

The spec for this scenario (`spikes/nspawn/spike_test.go`) stops the machine
the moment `machinectl show` says `State=running`, ~100 ms after start. That
stop took 90 s and left the machine behind:

```
# the spec stopped the machine ~100 ms after State=running; nspawn never shut the container down, timed out after 90 s, was SIGABRTed by the Fedora drop-in, and the payload lived on in mill-e0.scope
$ machinectl status mill-e0 --no-pager | head -6; systemctl is-active mill-machine@mill-e0.service mill-e0.scope
mill-e0(e7462a43cffa429ea194ef3a62d7479d)
	   Since: Fri 2026-09-11 00:20:18 PDT; 2min 27s ago
	  Leader: 2189922 (systemd)
	 Superv.: 2189919
	 Service: systemd-nspawn; class container
	    Root: /var/lib/machines/.#machine.mill-image-spikee0b6c81c3f66d6e5
failed
active
$ sudo journalctl -u mill-machine@mill-e0.service --since "2026-09-11 00:21:40" --until "2026-09-11 00:21:50" --no-pager -o short-precise | grep -v "^\s*$" | grep -v "Module \|#[0-9]"
Sep 11 00:21:48.861353 core systemd[1]: mill-machine@mill-e0.service: State 'stop-sigterm' timed out. Aborting.
Sep 11 00:21:48.861569 core systemd[1]: mill-machine@mill-e0.service: Killing process 2189919 (systemd-nspawn) with signal SIGABRT.
Sep 11 00:21:48.929108 core systemd-coredump[2193388]: Process 2189919 (systemd-nspawn) of user 0 dumped core.
Sep 11 00:21:48.941930 core systemd-nspawn[2189919]: mill-e0 login:
Sep 11 00:21:48.942203 core systemd[1]: mill-machine@mill-e0.service: Main process exited, code=dumped, status=6/ABRT
Sep 11 00:21:48.942236 core systemd[1]: mill-machine@mill-e0.service: Failed with result 'timeout'.
Sep 11 00:21:48.942391 core systemd[1]: Stopped mill-machine@mill-e0.service.
$ time sudo machinectl terminate mill-e0; machinectl list --no-pager
sudo machinectl terminate mill-e0  0.00s user 0.00s system 84% cpu 0.008 total
No machines.
# reproduce: start, stop the moment machined says running
$ sudo systemctl start mill-machine@mill-e0.service; until [ "$(machinectl show mill-e0 -p State 2>/dev/null)" = State=running ]; do sleep 0.02; done; time sudo systemctl stop mill-machine@mill-e0.service; machinectl list --no-pager
Job for mill-machine@mill-e0.service failed because the control process exited with error code.
See "systemctl status mill-machine@mill-e0.service" and "journalctl -xeu mill-machine@mill-e0.service" for details.
```

The unit's `KillMode=mixed` does not reach the payload: `--boot` registers
the container with machined, which puts it in its own `mill-e0.scope` under
`machine.slice`, outside the service's cgroup. When nspawn is SIGABRTed the
container's init and every service in it keep running there, `machinectl
list` keeps showing the machine, and — until the scope goes — a second start
of the same name is refused:

```
# and the aborted nspawn left its unix-export mount behind, so the next start of the same name refused:
$ sudo journalctl -u mill-machine@mill-e0.service --since "2026-09-11 00:22:47" --until "2026-09-11 00:22:48" --no-pager -o short-precise
Sep 11 00:22:47.422640 core systemd[1]: Starting mill-machine@mill-e0.service...
Sep 11 00:22:47.450616 core systemd-nspawn[2196325]: Mount point '/run/systemd/nspawn/unix-export/mill-e0' exists already, refusing.
Sep 11 00:22:47.452123 core systemd[1]: mill-machine@mill-e0.service: Main process exited, code=exited, status=1/FAILURE
Sep 11 00:22:47.452154 core systemd[1]: mill-machine@mill-e0.service: Failed with result 'exit-code'.
Sep 11 00:22:47.452276 core systemd[1]: Failed to start mill-machine@mill-e0.service.
$ findmnt /run/systemd/nspawn/unix-export/mill-e0; sudo umount /run/systemd/nspawn/unix-export/mill-e0; sudo rmdir /run/systemd/nspawn/unix-export/mill-e0; ls /run/systemd/nspawn/unix-export/
umount: /run/systemd/nspawn/unix-export/mill-e0: no mount point specified.
rmdir: failed to remove '/run/systemd/nspawn/unix-export/mill-e0': No such file or directory
```

Reproduced by hand, with the stop issued as soon as machined reports
`running`:

```
# reproduce: start, then stop the moment machined says running
$ sudo systemctl reset-failed mill-machine@mill-e0.service; sudo systemctl start mill-machine@mill-e0.service; echo "start rc=$?"; until [ "$(machinectl show mill-e0 -p State 2>/dev/null)" = State=running ]; do sleep 0.02; done; time timeout 120 sudo systemctl stop mill-machine@mill-e0.service; machinectl list --no-pager
start rc=0
timeout 120 sudo systemctl stop mill-machine@mill-e0.service  0.00s user 0.01s system 0% cpu 1:30.17 total
MACHINE CLASS     SERVICE        OS VERSION ADDRESSES
mill-e0 container systemd-nspawn -  -       -

1 machines listed.
```

`machinectl terminate` (through `org.freedesktop.machine1.manage-machines`,
so the operator's rule admits it) removes the orphan in 8 ms. The same stop
issued once the machine's bus answers, or once `is-system-running` returns,
takes a quarter of a second and shuts the machine down in order:

```
$ sudo machinectl terminate mill-e0; sleep 1; machinectl list --no-pager; sudo systemctl reset-failed mill-machine@mill-e0.service
No machines.
# the same stop after the machine reports ready
$ sudo systemctl start mill-machine@mill-e0.service; until sudo systemd-run -M mill-e0 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.1; done; sudo systemd-run -M mill-e0 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; time sudo systemctl stop mill-machine@mill-e0.service; machinectl list --no-pager
Job for mill-machine@mill-e0.service failed because the control process exited with error code.
See "systemctl status mill-machine@mill-e0.service" and "journalctl -xeu mill-machine@mill-e0.service" for details.
Failed to connect to system scope bus via machine transport: Host is down
sudo systemctl stop mill-machine@mill-e0.service  0.00s user 0.00s system 72% cpu 0.010 total
No machines.
# and a stop as soon as the bus answers, before is-system-running
$ sudo systemctl start mill-machine@mill-e0.service; until sudo systemd-run -M mill-e0 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.05; done; time timeout 120 sudo systemctl stop mill-machine@mill-e0.service; machinectl list --no-pager
timeout 120 sudo systemctl stop mill-machine@mill-e0.service  0.00s user 0.00s system 3% cpu 0.240 total
No machines.
```

(The first start in that transcript failed because the `until` loop's
`systemd-run -M` matched the terminated machine's dying registration; the
second and third are the measurements.) A clean run of the after-ready stop:

```
# the same stop after the machine reports ready
$ sudo systemctl start mill-machine@mill-e0.service; sleep 2; sudo systemd-run -M mill-e0 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; time sudo systemctl stop mill-machine@mill-e0.service; machinectl list --no-pager
running
sudo systemctl stop mill-machine@mill-e0.service  0.00s user 0.00s system 2% cpu 0.277 total
No machines.
$ sudo journalctl -u mill-machine@mill-e0.service --since "2026-09-11 00:32:00" --until "2026-09-11 00:32:10" --no-pager -o short-precise | grep -E "Trying to halt|Reached target halt|Halting|Deactivated|Stopped mill-machine"
Sep 11 00:32:04.325144 core systemd-nspawn[2256653]: Trying to halt container by sending TERM to container PID 1. Send SIGTERM again to trigger immediate termination.
Sep 11 00:32:04.411134 core systemd-nspawn[2256653]: [  OK  ] Reached target halt.target - System Halt.
Sep 11 00:32:04.454503 core systemd-nspawn[2256653]: Halting system.
Sep 11 00:32:04.552935 core systemd[1]: mill-machine@mill-e0.service: Deactivated successfully.
Sep 11 00:32:04.553060 core systemd[1]: Stopped mill-machine@mill-e0.service.
```

The signal nspawn sends the container's init (`SIGRTMIN+3`; the log line
says `TERM`) is lost if it arrives before the container's systemd has
installed its handlers, and nspawn then waits for a shutdown that never
begins. §Destroy must not rely on `systemctl stop` alone: mill should not
stop a machine before its bus has answered once, and the destroy path — in
particular the one a readiness timeout takes — needs `machinectl terminate`
after a bounded `systemctl stop`, or the template needs
`ExecStopPost=-machinectl terminate %i` and a `TimeoutStopSec=` shorter than
Fedora's 90 s.

**Verdict: E0 fails. Start and stop succeed as the operator through the polkit template, and a unit outside the template (a new name and an existing one) is refused; exec does not — `systemd-run -M` and `systemctl -M` need `CAP_SYS_ADMIN` regardless of polkit, and `machinectl shell`, the only polkit-mediated entry, returns no exit status and merges stderr. The fallback of `docs/sandbox.md` §Privilege applies: a root-owned helper for exec (and for the slice stop), with start and stop either staying on the polkit template or joining the helper as its other two verbs. A stop before the machine's bus is up hangs for `TimeoutStopSec` and orphans the payload in `mill-e0.scope`; `machinectl terminate` clears it.**

---

## E1 — Boot an ephemeral machine and exec into it

With `sudo`, as the work order gives it. Timestamps are `date +%T.%N`.

```
$ date +%T.%N; sudo systemd-run --unit=mill-run-e1 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false
23:51:30.826943641
Running as unit: mill-run-e1.service; invocation ID: 73738a3db9b5412ab3b00eb85360804c
$ until [ "$(sudo machinectl show mill-e1 -p State 2>/dev/null)" = State=running ]; do sleep 0.1; done; date +%T.%N; sudo machinectl show mill-e1 -p State
23:51:30.961867798
State=running
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; echo "rc=$?"
Failed to connect to system scope bus via machine transport: No such file or directory
rc=1
23:51:30.978629876
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'echo out; echo err >&2; exit 42' 2>/tmp/spike-out/e1.stderr; echo "rc=$?"; echo "--- stderr:"; cat /tmp/spike-out/e1.stderr
rc=1
--- stderr:
Failed to connect to system scope bus via machine transport: No such file or directory
$ sudo btrfs subvolume list /var/lib/machines
ID 256 gen 16 top level 5 path @mill
ID 257 gen 21 top level 5 path @machines
ID 265 gen 17 top level 257 path mill-image-spike
ID 267 gen 21 top level 257 path .#machine.mill-image-spikefabc4667bcdec177
$ sudo ausearch -m avc -ts recent
----
time->Thu Sep 10 23:47:53 2026
type=AVC msg=audit(1789109273.219:20005): avc:  denied  { search } for  pid=2104030 comm="prio-rpc-virtqe" name="2104028" dev="proc" ino=200755946 scontext=system_u:system_r:virtqemud_t:s0 tcontext=system_u:system_r:unconfined_service_t:s0 tclass=dir permissive=1
----
time->Thu Sep 10 23:47:53 2026
type=AVC msg=audit(1789109273.219:20006): avc:  denied  { read } for  pid=2104030 comm="prio-rpc-virtqe" name="stat" dev="proc" ino=200757560 scontext=system_u:system_r:virtqemud_t:s0 tcontext=system_u:system_r:unconfined_service_t:s0 tclass=file permissive=1
----
time->Thu Sep 10 23:47:53 2026
type=AVC msg=audit(1789109273.219:20007): avc:  denied  { open } for  pid=2104030 comm="prio-rpc-virtqe" path="/proc/2104028/stat" dev="proc" ino=200757560 scontext=system_u:system_r:virtqemud_t:s0 tcontext=system_u:system_r:unconfined_service_t:s0 tclass=file permissive=1
----
time->Thu Sep 10 23:47:53 2026
type=AVC msg=audit(1789109273.219:20008): avc:  denied  { search } for  pid=2104030 comm="prio-rpc-virtqe" name="2104028" dev="proc" ino=200755946 scontext=system_u:system_r:virtqemud_t:s0 tcontext=system_u:system_r:unconfined_service_t:s0 tclass=dir permissive=1
```

`State=running` came 135 ms after `systemd-run` returned, and it is machined's
state, not the machine's: for another ~200 ms there is no
`/run/dbus/system_bus_socket` inside, and `systemd-run -M` fails with
`No such file or directory`. The readiness poll in §Provision must therefore
treat a failed connect as "not yet", not as an error. The retry, ten seconds
later, and the exec:

```
$ date +%T.%N; until sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.2; done; date +%T.%N
23:51:41.532539597
23:51:41.579998455
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; echo "rc=$?"
running
rc=0
23:51:41.612026042
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'echo out; echo err >&2; exit 42' 2>/tmp/spike-out/e1.stderr; echo "rc=$?"; echo "--- stderr:"; cat /tmp/spike-out/e1.stderr
out
rc=42
--- stderr:
err
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- systemctl --failed --no-pager
  UNIT LOAD ACTIVE SUB DESCRIPTION

0 loaded units listed.
$ sudo ausearch -m avc -ts 23:50:00
<no matches>
```

`out` on stdout, `err` on stderr (captured to a file to prove the split), and
`rc=42`. The snapshot `.#machine.mill-image-spikefabc4667bcdec177` (ID 267,
top level 257) is `--ephemeral`'s, and vanishes when the machine stops. The
only AVC records since the session began are `virtqemud_t` ones from 23:47,
before the first machine, in a permissive domain, about libvirt's own
processes; none from the machine.

A later boot, timed from `systemd-run` to a reachable bus and to
`is-system-running`:

```
$ sudo systemctl stop mill-run-e1; sudo machinectl list --no-pager
No machines.
# reboot the machine and this time measure start → bus reachable
$ t0=$(date +%s.%N); sudo systemd-run --unit=mill-run-e1 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false; until [ "$(sudo machinectl show mill-e1 -p State 2>/dev/null)" = State=running ]; do sleep 0.05; done; t1=$(date +%s.%N); until sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.1; done; t2=$(date +%s.%N); sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; t3=$(date +%s.%N); echo "State=running after $(echo "$t1 - $t0" | bc)s; bus reachable after $(echo "$t2 - $t0" | bc)s; is-system-running after $(echo "$t3 - $t0" | bc)s"
Running as unit: mill-run-e1.service; invocation ID: 3a49ee53f0f745e8b3f5cd54effb8af4
running
State=running after .079401336s; bus reachable after .311295711s; is-system-running after .335617657s
```

**Verdict: E1 passes. `State=running`; `is-system-running --wait` returns `running`; `out` on stdout, `err` on stderr, `rc=42`; the ephemeral snapshot is a subvolume under `/var/lib/machines` while the machine runs; `ausearch` shows no denial from the machine. Boot to a usable bus is a third of a second.**

---

## E2 — Podman runs inside the machine

### First attempt: `newuidmap` cannot write the map

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman info --format '{{.Store.GraphDriverName}} {{.Host.CgroupManager}}'; echo "rc=$?"
time="2026-09-10T23:51:50-07:00" level=error msg="running `/usr/bin/newuidmap 129 0 1000 1 1 524288 65536`: newuidmap: write to uid_map failed: Operation not permitted\n"
Error: cannot set up namespace using "/usr/bin/newuidmap": should have setuid or have filecaps setuid: exit status 1
rc=125
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello; echo "rc=$?"
time="2026-09-10T23:51:50-07:00" level=error msg="running `/usr/bin/newuidmap 151 0 1000 1 1 524288 65536`: newuidmap: write to uid_map failed: Operation not permitted\n"
Error: cannot set up namespace using "/usr/bin/newuidmap": should have setuid or have filecaps setuid: exit status 1
rc=125
$ sudo ausearch -m avc -ts 23:51:00
<no matches>
```

Two causes, found one after the other.

**Cause one: the subordinate range does not fit the machine.** `useradd`
gave `agent` `524288:65536`. The machine's own user namespace is 65536 uids
wide (`--private-users=pick`), so the kernel refuses a map that reaches past it:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'cat /etc/subuid /etc/subgid; getcap /usr/bin/newuidmap; cat /proc/self/uid_map'
agent:524288:65536
agent:524288:65536
         0  946929664      65536
# fix attempt: shrink agent's subordinate ranges to fit inside the machine's 65536 uids
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'echo agent:10000:55536 > /etc/subuid; echo agent:10000:55536 > /etc/subgid'
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman system migrate
time="2026-09-10T23:52:09-07:00" level=error msg="running `/usr/bin/newuidmap 184 0 1000 1 1 10000 55536`: newuidmap: write to uid_map failed: Operation not permitted\n"
Error: cannot set up namespace using "/usr/bin/newuidmap": should have setuid or have filecaps setuid: exit status 1
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman info --format '{{.Store.GraphDriverName}} {{.Host.CgroupManager}}'; echo "rc=$?"
time="2026-09-10T23:52:09-07:00" level=error msg="running `/usr/bin/newuidmap 204 0 1000 1 1 10000 55536`: newuidmap: write to uid_map failed: Operation not permitted\n"
Error: cannot set up namespace using "/usr/bin/newuidmap": should have setuid or have filecaps setuid: exit status 1
rc=125
```

Shrinking the range to `10000:55536` — inside the machine's 65536 — did not
change the error, which pointed at the second cause.

**Cause two: `podman export` drops file capabilities.** `rpm -V` reports the
capabilities (`P`) differ; `getcap` prints nothing inside the machine and
`cap_setuid=ep` on the host; the OCI image built by rootless podman already
lacks them:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'ls -l /usr/bin/newuidmap /usr/bin/newgidmap; getcap -v /usr/bin/newuidmap; rpm -q --qf "%{NAME} " shadow-utils; rpm -V shadow-utils | grep -i map'
-rwxr-xr-x. 1 root root 43320 Apr 28 17:00 /usr/bin/newgidmap
-rwxr-xr-x. 1 root root 39208 Apr 28 17:00 /usr/bin/newuidmap
/usr/bin/newuidmap
shadow-utils ........P    /usr/bin/newgidmap
........P    /usr/bin/newuidmap
$ ls -l /usr/bin/newuidmap; getcap /usr/bin/newuidmap   # on the host, for comparison
-rwxr-xr-x. 1 root root 39208 Apr 28 17:00 /usr/bin/newuidmap
/usr/bin/newuidmap cap_setuid=ep
$ podman run --rm --signature-policy /dev/null localhost/mill-base-spike getcap /usr/bin/newuidmap   # in the OCI image before export
-rwxr-xr-x. 1 root root 39208 Apr 29 00:00 /usr/bin/newuidmap
```

The fix, applied to the image subvolume for later boots and to the running
ephemeral copy:

```
# fix: restore the file capabilities podman export dropped
$ sudo setcap cap_setuid=ep /var/lib/machines/mill-image-spike/usr/bin/newuidmap && sudo setcap cap_setgid=ep /var/lib/machines/mill-image-spike/usr/bin/newgidmap   # the image subvolume, for later boots
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'setcap cap_setuid=ep /usr/bin/newuidmap && setcap cap_setgid=ep /usr/bin/newgidmap && getcap /usr/bin/newuidmap /usr/bin/newgidmap'   # the running ephemeral copy
/usr/bin/newuidmap cap_setuid=ep
/usr/bin/newgidmap cap_setgid=ep
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman info --format '{{.Store.GraphDriverName}} {{.Host.CgroupManager}}'; echo "rc=$?"
overlay systemd
rc=0
```

`overlay systemd`. To show that both causes are real, the original range with
the capability restored:

```
# does the original 524288:65536 range work now that newuidmap has its caps?
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'echo agent:524288:65536 > /etc/subuid; echo agent:524288:65536 > /etc/subgid'
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'podman system migrate; podman info --format "{{.Store.GraphDriverName}} {{.Host.CgroupManager}}"'; echo "rc=$?"
time="2026-09-10T23:52:52-07:00" level=error msg="running `/usr/bin/newuidmap 325 0 1000 1 1 524288 65536`: newuidmap: write to uid_map failed: Operation not permitted\n"
Error: fatal error, invalid internal status, unable to create a new pause process: cannot set up namespace using "/usr/bin/newuidmap": exit status 1. Try running "podman system migrate" and if that doesn't work reboot to recover
rc=125
# back to the range that fits the machine
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'echo agent:10000:55536 > /etc/subuid; echo agent:10000:55536 > /etc/subgid'
```

Both fixes are in the committed artefacts: the range in
`Containerfile.spike`, the `setcap` in `build-image.sh`.

### Second attempt: no route to a registry

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello; echo "rc=$?"
Trying to pull quay.io/podman/hello:latest...
Error: unable to copy from source docker://quay.io/podman/hello:latest: initializing source docker://quay.io/podman/hello:latest: pinging container registry harbor.tailcbcc0e.ts.net: Get "https://harbor.tailcbcc0e.ts.net/v2/": dial tcp: lookup harbor.tailcbcc0e.ts.net: Temporary failure in name resolution
rc=125
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'ip -br addr; ip route; cat /etc/resolv.conf'
sh: line 1: ip: command not found
sh: line 1: ip: command not found
# This is /run/systemd/resolve/stub-resolv.conf managed by man:systemd-resolved(8).
# Do not edit.
#
# This file might be symlinked as /etc/resolv.conf. If you're looking at
# /etc/resolv.conf and seeing this text, you have followed the symlink.
#
# This is a dynamic resolv.conf file for connecting local clients to the
# internal DNS stub resolver of systemd-resolved. This file lists all
# configured search domains.
#
# Run "resolvectl status" to see details about the uplink DNS servers
# currently in use.
#
# Third party programs should typically not access this file directly, but only
# through the symlink at /etc/resolv.conf. To manage man:resolv.conf(5) in a
# different way, replace this symlink by a static file or a different symlink.
#
# See man:systemd-resolved.service(8) for details about the supported modes of
# operation for /etc/resolv.conf.

nameserver 127.0.0.53
options edns0 trust-ad
search .
```

The machine has no address (NetworkManager on the host does not configure the
`ve-*` side; the next spike node owns that) and Harbor was answering 502 in any
case. The image was loaded by hand:

```
# the machine has no route out (network is the next spike node) and Harbor answers 502 tonight;
# the host fetches hello straight from quay.io, bypassing its registries.conf, and pipes it into agent's store
$ curl -sS -m 10 -o /dev/null -w "harbor %{http_code}
" https://harbor.tailcbcc0e.ts.net/v2/
harbor 502
$ CONTAINERS_REGISTRIES_CONF=/dev/null podman pull quay.io/podman/hello
Trying to pull quay.io/podman/hello:latest...
Getting image source signatures
Copying blob sha256:81df7ff16254ed9756e27c8de9ceb02a9568228fccadbf080f41cc5eb5118a44
Copying config sha256:5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Writing manifest to image destination
5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
$ podman save quay.io/podman/hello | sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman load
Getting image source signatures
Copying blob sha256:2114fc8b70586b9325dde6fd26066d9951414dcdfb3995f41d51d1995cf3bd9d
Copying config sha256:5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Writing manifest to image destination
Loaded image: quay.io/podman/hello:latest
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello; echo "rc=$?"
Error: crun: mount `proc` to `proc`: Operation not permitted: OCI permission denied
rc=126
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'id; podman info --format "{{.Host.Security.Rootless}} {{.Host.OCIRuntime.Name}} {{.Host.NetworkBackend}} {{.Host.Security.SELinuxEnabled}}"'
uid=1000(agent) gid=1000(agent) groups=1000(agent)
rootless=true runtime=crun network=netavark selinux=false
$ sudo ausearch -m avc -ts 23:51:00
<no matches>
```

### Third attempt: crun cannot mount `/proc`

`podman info` works, `podman run` does not: `crun: mount 'proc' to 'proc':
Operation not permitted`. No AVC. The cause is nspawn's `/proc` hardening. With
`--private-users`, nspawn always bind-mounts read-only or inaccessible copies
over files and directories under `/proc` (`src/nspawn/nspawn-mount.c`
`mount_all()`: `PROC_INACCESSIBLE_REG` for `kallsyms kcore keys sysrq-trigger
timer_list`, `PROC_READ_ONLY` for `acpi apm asound bus fs irq scsi`, plus
`/proc/sys` and `/proc/kmsg`; `SYSTEMD_NSPAWN_API_VFS_WRITABLE=1` would skip
them but nspawn refuses it together with `--private-users`). When `agent`'s
rootless podman creates its user namespace, every mount it inherits becomes
`MNT_LOCKED`, and the kernel's `mnt_already_visible()` (`fs/namespace.c`)
refuses a fresh `proc` mount in a namespace whose existing `proc` has locked
children over anything but empty directories. What the machine sees:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'grep " /proc" /proc/self/mountinfo'
4350 4303 0:428 / /proc rw,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4379 4350 0:428 /sys /proc/sys ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4400 4379 0:428 /sys/net /proc/sys/net rw,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4401 4350 0:428 /sys/net /proc/sys/net rw,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4402 4350 0:424 /host/inaccessible/reg /proc/kallsyms ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4403 4350 0:424 /host/inaccessible/reg /proc/kcore ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4404 4350 0:424 /host/inaccessible/reg /proc/keys ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4405 4350 0:424 /host/inaccessible/reg /proc/sysrq-trigger ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4406 4350 0:424 /host/inaccessible/reg /proc/timer_list ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4408 4350 0:428 /acpi /proc/acpi ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4409 4350 0:428 /asound /proc/asound ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4410 4350 0:428 /bus /proc/bus ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4411 4350 0:428 /fs /proc/fs ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4412 4350 0:428 /irq /proc/irq ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4413 4350 0:428 /scsi /proc/scsi ro,nosuid,nodev,noexec,relatime shared:1942 - proc proc rw
4422 4379 0:424 /host/proc-sys-kernel-random-boot-id /proc/sys/kernel/random/boot_id ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4423 4350 0:424 /host/proc-sys-kernel-random-boot-id /proc/sys/kernel/random/boot_id ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
4424 4350 0:424 /host/proc-kmsg /proc/kmsg ro,nosuid,nodev,noexec shared:1936 - tmpfs tmpfs rw,seclabel,size=6395848k,nr_inodes=819200,mode=755,uid=946929664,gid=946929664,inode64
```

The mounts were made *inside* the machine's user namespace, so the machine's
root owns them and may remove them. First pass — everything under `/proc`,
in reverse order:

```
# is it the proc overmounts? root inside the machine (unprivileged on the host, but owner of the machine's mount namespace) can drop them
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'for p in $(awk "\$5 ~ /^\/proc\// {print \$5}" /proc/self/mountinfo | sort -r); do umount "$p" && echo "umounted $p"; done; grep -c " /proc" /proc/self/mountinfo'
umounted /proc/timer_list
umounted /proc/sysrq-trigger
umounted /proc/sys/net
umount: /proc/sys/net: not mounted.
umounted /proc/sys/kernel/random/boot_id
umount: /proc/sys/kernel/random/boot_id: not mounted.
umount: /proc/sys: target is busy.
umounted /proc/scsi
umounted /proc/kmsg
umounted /proc/keys
umounted /proc/kcore
umounted /proc/kallsyms
umounted /proc/irq
umounted /proc/fs
umounted /proc/bus
umounted /proc/asound
umounted /proc/acpi
2
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello; echo "rc=$?"
Error: current system boot ID differs from cached boot ID; an unhandled reboot has occurred. Please delete directories "/run/user/1000/containers" and "/run/user/1000/libpod/tmp" and re-run Podman
rc=125
```

Removing `/proc/sys/kernel/random/boot_id` changed the boot id podman had
cached; `/proc/sys` was busy. Cleaning podman's runtime directory:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'grep " /proc" /proc/self/mountinfo | cut -d" " -f5,6; umount /proc/sys/net; umount /proc/sys; echo "umount /proc/sys rc=$?"; grep " /proc" /proc/self/mountinfo | cut -d" " -f5,6'
/proc rw,nosuid,nodev,noexec,relatime
/proc/sys ro,nosuid,nodev,noexec,relatime
umount: /proc/sys/net: not mounted.
umount: /proc/sys: target is busy.
umount /proc/sys rc=32
/proc rw,nosuid,nodev,noexec,relatime
/proc/sys ro,nosuid,nodev,noexec,relatime
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'rm -rf /run/user/1000/containers /run/user/1000/libpod/tmp; podman run --rm quay.io/podman/hello'; echo "rc=$?"
Error: OCI runtime error: crun: open `/proc/sys/net/ipv4/ping_group_range`: Read-only file system
rc=126
```

The `proc` mount now succeeds — the error moved on to
`net.ipv4.ping_group_range`, read-only because the writable `/proc/sys/net`
bind had gone with the rest. From here the machine was rebooted (that boot is
the timed one in E1) to find the minimal set.

Fresh boot, only the seven file overmounts removed:

```
# fresh boot; unmount only the seven file overmounts, keep the read-only directory binds and both /proc/sys/net mounts
$ /tmp/spike-out/reboot-e1.sh   # stop, boot again, wait for the bus (E1's command)
Running as unit: mill-run-e1.service; invocation ID: 2238218cd6234250a43cadb195b909ab
running
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'for p in /proc/kallsyms /proc/kcore /proc/keys /proc/sysrq-trigger /proc/timer_list /proc/kmsg /proc/sys/kernel/random/boot_id; do umount -v "$p"; done; grep " /proc" /proc/self/mountinfo | cut -d" " -f1,2,5,6'
umount: /proc/kallsyms (tmpfs) unmounted
umount: /proc/kcore (tmpfs) unmounted
umount: /proc/keys (tmpfs) unmounted
umount: /proc/sysrq-trigger (tmpfs) unmounted
umount: /proc/timer_list (tmpfs) unmounted
umount: /proc/kmsg (tmpfs) unmounted
umount: /proc/sys/kernel/random/boot_id (tmpfs) unmounted
4350 4303 /proc rw,nosuid,nodev,noexec,relatime
4379 4350 /proc/sys ro,nosuid,nodev,noexec,relatime
4400 4379 /proc/sys/net rw,nosuid,nodev,noexec,relatime
4401 4350 /proc/sys/net rw,nosuid,nodev,noexec,relatime
4407 4350 /proc/acpi ro,nosuid,nodev,noexec,relatime
4408 4350 /proc/asound ro,nosuid,nodev,noexec,relatime
4409 4350 /proc/bus ro,nosuid,nodev,noexec,relatime
4410 4350 /proc/fs ro,nosuid,nodev,noexec,relatime
4411 4350 /proc/irq ro,nosuid,nodev,noexec,relatime
4412 4350 /proc/scsi ro,nosuid,nodev,noexec,relatime
$ podman save quay.io/podman/hello | sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman load
Getting image source signatures
Copying blob sha256:2114fc8b70586b9325dde6fd26066d9951414dcdfb3995f41d51d1995cf3bd9d
Copying config sha256:5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Writing manifest to image destination
Loaded image: quay.io/podman/hello:latest
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello; echo "rc=$?"
Error: crun: mount `proc` to `proc`: Operation not permitted: OCI permission denied
rc=126
```

Then the read-only directory binds as well, leaving `/proc/sys` and the two
`/proc/sys/net` mounts:

```
# same boot: also drop the read-only directory binds, keep /proc/sys and both /proc/sys/net
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'for p in /proc/acpi /proc/asound /proc/bus /proc/fs /proc/irq /proc/scsi; do umount -v "$p"; done; grep " /proc" /proc/self/mountinfo | cut -d" " -f1,2,5,6'
umount: /proc/acpi unmounted
umount: /proc/asound unmounted
umount: /proc/bus unmounted
umount: /proc/fs unmounted
umount: /proc/irq unmounted
umount: /proc/scsi unmounted
4350 4303 /proc rw,nosuid,nodev,noexec,relatime
4379 4350 /proc/sys ro,nosuid,nodev,noexec,relatime
4400 4379 /proc/sys/net rw,nosuid,nodev,noexec,relatime
4401 4350 /proc/sys/net rw,nosuid,nodev,noexec,relatime
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'podman system migrate; podman run --rm quay.io/podman/hello'; echo "rc=$?"
Error: crun: mount `proc` to `proc`: Operation not permitted: OCI permission denied
rc=126
```

Still refused. The two `/proc/sys/net` entries are nspawn's bind (4400, under
the `/proc/sys` bind) and its propagated twin (4401, a direct child of `/proc`
hidden beneath `/proc/sys`, which path-based `umount` cannot reach):

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'set -x; grep " /proc/sys" /proc/self/mountinfo | cut -d" " -f1,2,5,6; umount -v /proc/sys/net; grep " /proc/sys" /proc/self/mountinfo | cut -d" " -f1,2,5,6; umount -v /proc/sys/net; grep " /proc/sys" /proc/self/mountinfo | cut -d" " -f1,2,5,6'
+ grep ' /proc/sys' /proc/self/mountinfo
+ cut '-d ' -f1,2,5,6
4379 4350 /proc/sys ro,nosuid,nodev,noexec,relatime
4401 4350 /proc/sys/net rw,nosuid,nodev,noexec,relatime
4400 4379 /proc/sys/net rw,nosuid,nodev,noexec,relatime
+ umount -v /proc/sys/net
umount: /proc/sys/net unmounted
+ grep ' /proc/sys' /proc/self/mountinfo
+ cut '-d ' -f1,2,5,6
4379 4350 /proc/sys ro,nosuid,nodev,noexec,relatime
4401 4350 /proc/sys/net rw,nosuid,nodev,noexec,relatime
+ umount -v /proc/sys/net
umount: /proc/sys/net: not mounted.
+ grep ' /proc/sys' /proc/self/mountinfo
+ cut '-d ' -f1,2,5,6
4379 4350 /proc/sys ro,nosuid,nodev,noexec,relatime
4401 4350 /proc/sys/net rw,nosuid,nodev,noexec,relatime
```

Detaching `/proc/sys` lazily exposes and removes the rest. With `/proc` bare,
hello runs, and a container with a shell shows its own fresh `proc` with
podman's usual masks:

```
# strip /proc of every overmount: the /proc/sys bind is busy, so detach it lazily, which exposes the propagated shadow /proc/sys/net for a plain umount
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'umount -lv /proc/sys; umount -v /proc/sys/net; grep " /proc" /proc/self/mountinfo | cut -d" " -f1,2,5,6'
umount: /proc/sys (proc) unmounted
umount: /proc/sys/net: not mounted.
4350 4303 /proc rw,nosuid,nodev,noexec,relatime
$ podman save docker.io/library/redis:7-alpine | sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman load   # an image with a shell, to look at /proc from inside
Getting image source signatures
Copying blob sha256:31a8b9071ebe9ba8e62c01b042bedd011e79aff5c98b76a48ffd79f2802d1606
Copying blob sha256:b1c637855b5efbf089a1d2c9bdbd6e8f7b2e97009f4143ee6b01e6184eddd84d
Copying blob sha256:23e75c9dd3ec742dcf0b41994dd6e581b011fb29044efa7b1921f92f59e3e11e
Copying blob sha256:7a0883345acc5207b63846a91613da2c68c68246ab0cd6daba706657e97ab053
Copying blob sha256:eb1d2844db65db6755dea0d31b536f50c8a9a13b79836e6272d630bee843fe39
Copying blob sha256:2a0303fe4b8039a7289c44ea66b28dea7c9c7ed0d8ec3e127c69bf0cfc79d3f0
Copying blob sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef
Copying blob sha256:2f1d3b227ce778a95b5b5838e98d735992713f3c68be3fc4df7a031862ec5ebb
Copying config sha256:e08bd8d5a6774025cff22e5db8361f6a5d023811e84d77d0033e47d44970cc94
Writing manifest to image destination
Loaded image: docker.io/library/redis:7-alpine
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'podman system migrate; podman run --rm quay.io/podman/hello'; echo "rc=$?"
!... Hello Podman World ...!

         .--"--.           
       / -     - \         
      / (O)   (O) \        
   ~~~| -=(,Y,)=- |         
    .---. /`  \   |~~      
 ~/  o  o \~~~~.----. ~~   
  | =(X)= |~  / (O (O) \   
   ~~~~~~~  ~| =(Y_)=-  |   
  ~~~~    ~~~|   U      |~~ 

Project:   https://github.com/containers/podman
Website:   https://podman.io
Desktop:   https://podman-desktop.io
Documents: https://docs.podman.io
YouTube:   https://youtube.com/@Podman
X/Twitter: @Podman_io
Mastodon:  @Podman_io@fosstodon.org
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm docker.io/library/redis:7-alpine sh -c 'cat /proc/1/comm; grep " /proc" /proc/self/mountinfo | cut -d" " -f5,6,9'; echo "rc=$?"
sh
/proc rw,nosuid,nodev,noexec,relatime proc
/proc/acpi ro,nosuid,nodev,relatime tmpfs
/proc/kcore ro,nosuid tmpfs
/proc/keys ro,nosuid tmpfs
/proc/latency_stats ro,nosuid tmpfs
/proc/scsi ro,nosuid,nodev,relatime tmpfs
/proc/timer_list ro,nosuid tmpfs
/proc/interrupts ro,nosuid tmpfs
/proc/asound ro,nosuid,nodev,noexec,relatime proc
/proc/bus ro,nosuid,nodev,noexec,relatime proc
/proc/fs ro,nosuid,nodev,noexec,relatime proc
/proc/irq ro,nosuid,nodev,noexec,relatime proc
/proc/sys ro,nosuid,nodev,noexec,relatime proc
/proc/sysrq-trigger ro,nosuid,nodev,noexec,relatime proc
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'ls -l /proc/kcore /proc/sysrq-trigger /proc/sys/kernel/sysrq; test -w /proc/sysrq-trigger; echo "sysrq-trigger writable rc=$?"; test -r /proc/kcore; echo "kcore readable rc=$?"; head -c1 /proc/kcore >/dev/null; echo "kcore read rc=$?"; cat /proc/sys/kernel/sysrq > /tmp/v; echo "$(cat /tmp/v)" > /proc/sys/kernel/sysrq; echo "kernel.sysrq write rc=$?"; cat /proc/sys/net/ipv4/ping_group_range > /tmp/v; echo "$(cat /tmp/v)" > /proc/sys/net/ipv4/ping_group_range; echo "net.ipv4.ping_group_range write rc=$?"'   # what machine root can reach on the unmasked /proc (writes rewrite the current value)
-r--------. 1 nobody nobody 140737471598592 Sep 10 23:58 /proc/kcore
-rw-r--r--. 1 nobody nobody               0 Sep 10 23:58 /proc/sys/kernel/sysrq
--w-------. 1 nobody nobody               0 Sep 10 23:58 /proc/sysrq-trigger
sysrq-trigger writable rc=1
kcore readable rc=1
head: cannot open '/proc/kcore' for reading: Permission denied
kcore read rc=1
sh: line 1: /proc/sys/kernel/sysrq: Permission denied
kernel.sysrq write rc=1
net.ipv4.ping_group_range write rc=0
$ sudo ausearch -m avc -ts 23:51:00
<no matches>
```

The last command shows what unmasking costs inside a `--private-users`
machine: nothing the kernel does not already refuse. `kcore`, `sysrq-trigger`
and the non-network sysctls belong to the host's root (`nobody` inside) and
DAC denies the machine's root; `net.*` sysctls are the network namespace's own
and were writable before.

### The recipe: one mount

Rather than unmounting thirteen paths, mount a pristine `proc` over the masked
one. `mnt_already_visible()` accepts the new mount if *any* fully-rooted `proc`
mount in the namespace has no locked non-empty children; the stacked one has
none. Fresh boot, one command, hello:

```
# a one-command alternative: stack a pristine proc over the masked one; the kernel accepts a new proc mount in agent's namespace if any proc mount is fully visible
$ /tmp/spike-out/reboot-e1.sh
Running as unit: mill-run-e1.service; invocation ID: 360c19c5236f42eeab098cdc26194809
running
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'mount -t proc proc /proc; echo "mount rc=$?"; grep " /proc" /proc/self/mountinfo | cut -d" " -f1,2,5,6 | tail -3'
mount rc=0
4347 4350 /proc/sys/kernel/random/boot_id ro,nosuid,nodev,noexec
4348 4350 /proc/kmsg ro,nosuid,nodev,noexec
4776 4350 /proc rw,relatime
$ podman save quay.io/podman/hello | sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman load >/dev/null
Getting image source signatures
Copying blob sha256:2114fc8b70586b9325dde6fd26066d9951414dcdfb3995f41d51d1995cf3bd9d
Copying config sha256:5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Writing manifest to image destination
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman run --rm quay.io/podman/hello | head -3; echo "rc=${pipestatus[1]}"
!... Hello Podman World ...!

         .--"--.           
rc=0
```

(The rebuilt image in §Setup uses `-o nosuid,nodev,noexec` on that mount.)
This is a boot-time unit in the image for `mill#59`, or a line in
`homestead#33`'s template; the machine's root runs it before `agent`'s first
podman command, because podman's pause process copies the mount table when it
is created.

### AVC denials

Every `ausearch -m avc` in this section, and in the ones before and after, on
the window from the first machine boot:

```
$ sudo ausearch -m avc -ts 23:51:00
<no matches>
```

**Verdict: E2 passes. `podman info` prints `overlay systemd` and hello prints as `agent`, with no `--device /dev/fuse` and no capability or syscall flag on the outer invocation. It needed three things the design did not know: `newuidmap`/`newgidmap` file capabilities restored after `podman export`; `agent`'s subordinate ids inside the machine's 65536-uid range (`10000:55536`); and a pristine `proc` mounted over nspawn's masked one before podman's first run. `ausearch` returned none, so `homestead#35` has no denial to build policy from — SELinux needed neither a module nor a context flag.**

---

## E3 — A step slice reaps what a step spawned

### First attempt, as written

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run -d --name lingering quay.io/podman/hello sleep 3600; echo started'; echo "rc=$?"
Error: crun: executable file `sleep` not found in $PATH: No such file or directory: OCI runtime attempted to invoke a command that was not found
started
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'pgrep -a sleep; podman ps -a --format "{{.Names}} {{.Status}}"'
sh: line 1: pgrep: command not found
lingering Created
$ sudo systemd-cgls -M mill-e1 --no-pager
CGroup /machine.slice/mill-e1.scope:
-.slice
└─payload
  ├─user.slice
  │ └─user-1000.slice
  │   └─user@1000.service …
  │     ├─session.slice
  │     │ └─dbus-broker.service
  │     │   ├─2140430 /usr/bin/dbus-broker-launch --scope user
  │     │   └─2140431 dbus-broker --log 10 --controller 9 --machine-id f94084a2…
  │     ├─user.slice
  │     │ └─podman-pause-5d022444.scope
  │     │   └─2140415 catatonit -P
  │     └─init.scope
  │       ├─2140312 /usr/lib/systemd/systemd --user
  │       └─2140314 (sd-pam)…
  ├─init.scope
  │ └─2140195 /usr/lib/systemd/systemd
  └─system.slice
    ├─dbus-broker.service
    │ ├─2140294 /usr/bin/dbus-broker-launch --scope system --audit
    │ └─2140305 dbus-broker --log 10 --controller 9 --machine-id f94084a22f374f…
    ├─systemd-homed.service
    │ └─2140292 /usr/lib/systemd/systemd-homed
    ├─systemd-journald.service
    │ └─2140259 /usr/lib/systemd/systemd-journald
    ├─systemd-userdbd.service
    │ ├─2140266 /usr/lib/systemd/systemd-userdbd
    │ ├─2140309 systemd-userwork: waiting...
    │ ├─2140412 systemd-userwork: waiting...
    │ └─2140449 systemd-userwork: waiting...
    ├─console-getty.service
    │ └─2140308 /usr/sbin/agetty --noreset --noclear --issue-file=/etc/issue:/e…
    ├─systemd-oomd.service
    │ └─2140279 /usr/lib/systemd/systemd-oomd
    ├─systemd-resolved.service
    │ └─2140281 /usr/lib/systemd/systemd-resolved
    └─systemd-logind.service
      └─2140293 /usr/lib/systemd/systemd-logind
```

`quay.io/podman/hello` is a single static binary with no `sleep`, and the image
has no `pgrep` (procps-ng is not in the package list). Two things are already
visible in the cgroup tree: the `nohup sleep` is gone — the exec unit's default
`KillMode=control-group` killed it when `sh` exited — and podman's pause
process sits under `user@1000.service`, not under any step slice.

### Second attempt: a container that sleeps, and `/proc/*/comm` for `pgrep`

```
# quay.io/podman/hello carries no sleep and the image no pgrep; redis:7-alpine stands in for the container and /proc/*/comm for pgrep
$ podman save docker.io/library/redis:7-alpine | sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman load | tail -1; sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm lingering
Getting image source signatures
Copying blob sha256:b1c637855b5efbf089a1d2c9bdbd6e8f7b2e97009f4143ee6b01e6184eddd84d
Copying blob sha256:23e75c9dd3ec742dcf0b41994dd6e581b011fb29044efa7b1921f92f59e3e11e
Copying blob sha256:7a0883345acc5207b63846a91613da2c68c68246ab0cd6daba706657e97ab053
Copying blob sha256:eb1d2844db65db6755dea0d31b536f50c8a9a13b79836e6272d630bee843fe39
Copying blob sha256:31a8b9071ebe9ba8e62c01b042bedd011e79aff5c98b76a48ffd79f2802d1606
Copying blob sha256:2a0303fe4b8039a7289c44ea66b28dea7c9c7ed0d8ec3e127c69bf0cfc79d3f0
Copying blob sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef
Copying blob sha256:2f1d3b227ce778a95b5b5838e98d735992713f3c68be3fc4df7a031862ec5ebb
Copying config sha256:e08bd8d5a6774025cff22e5db8361f6a5d023811e84d77d0033e47d44970cc94
Writing manifest to image destination
Loaded image: docker.io/library/redis:7-alpine
lingering
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run -d --name lingering docker.io/library/redis:7-alpine sleep 3600; echo started'; echo "rc=$?"
116ca8b464dda7d1e8ec5c1d50aa26c46b1824c2480f904dde56acd9867bb0e6
started
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'grep -l "^sleep" /proc/[0-9]*/comm; podman ps --format "{{.Names}} {{.Status}}"'
/proc/572/comm
lingering Up Less than a second
$ sudo systemd-cgls -M mill-e1 --no-pager | grep -v "^ *[├└│ ]*[0-9]* systemd-userwork"
CGroup /machine.slice/mill-e1.scope:
-.slice
└─payload
  ├─user.slice
  │ └─user-1000.slice
  │   └─user@1000.service …
  │     ├─session.slice
  │     │ └─dbus-broker.service
  │     │   ├─2140430 /usr/bin/dbus-broker-launch --scope user
  │     │   └─2140431 dbus-broker --log 10 --controller 9 --machine-id f94084a2…
  │     ├─user.slice
  │     │ ├─libpod-116ca8b464dda7d1e8ec5c1d50aa26c46b1824c2480f904dde56acd9867bb0e6.scope
  │     │ │ └─container
  │     │ │   └─2142146 sleep 3600
  │     │ ├─podman-pause-5d022444.scope
  │     │ │ └─2140415 catatonit -P
  │     │ └─podman-540.scope
  │     │   ├─2142141 /usr/bin/pasta --config-net --dns-forward 169.254.1.1 -t …
  │     │   └─2142144 /usr/bin/conmon --api-version 1 -c 116ca8b464dda7d1e8ec5c…
  │     └─init.scope
  │       ├─2140312 /usr/lib/systemd/systemd --user
  │       └─2140314 (sd-pam)…
  ├─init.scope
  │ └─2140195 /usr/lib/systemd/systemd
  └─system.slice
    ├─dbus-broker.service
    │ ├─2140294 /usr/bin/dbus-broker-launch --scope system --audit
    │ └─2140305 dbus-broker --log 10 --controller 9 --machine-id f94084a22f374f…
    ├─systemd-homed.service
    │ └─2140292 /usr/lib/systemd/systemd-homed
    ├─systemd-journald.service
    │ └─2140259 /usr/lib/systemd/systemd-journald
    ├─systemd-userdbd.service
    │ ├─2140266 /usr/lib/systemd/systemd-userdbd
    │ ├─2140309 systemd-userwork: waiting...
    │ ├─2140412 systemd-userwork: waiting...
    │ └─2140449 systemd-userwork: waiting...
    ├─console-getty.service
    │ └─2140308 /usr/sbin/agetty --noreset --noclear --issue-file=/etc/issue:/e…
    ├─systemd-oomd.service
    │ └─2140279 /usr/lib/systemd/systemd-oomd
    ├─systemd-resolved.service
    │ └─2140281 /usr/lib/systemd/systemd-resolved
    └─systemd-logind.service
      └─2140293 /usr/lib/systemd/systemd-logind
$ sudo systemctl -M mill-e1 list-units --no-pager --no-legend "mill-step*"
  mill-step-1.slice loaded active active Slice /mill/step/1
  mill-step.slice   loaded active active Slice /mill/step
```

The container's cgroup, `libpod-116ca8….scope`, is under
`user.slice/user-1000.slice/user@1000.service`. Rootless podman with the systemd
cgroup manager asks *its user manager* for a scope, and the user manager cannot
place one under a system slice. Stop the slice and look:

```
$ sudo systemctl -M mill-e1 stop mill-step-1.slice; echo "rc=$?"
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-2.slice -- sh -c 'grep -l "^sleep" /proc/[0-9]*/comm; podman ps --format "{{.Names}} {{.Status}}"; echo done'
/proc/572/comm
lingering Up 14 seconds
done
# the container survived: it lives under user@1000.service, not the step slice. Clean up and retry with --cgroups=split under a delegated exec unit
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
time="2026-09-11T00:02:50-07:00" level=warning msg="StopSignal SIGTERM failed to stop container lingering in 10 seconds, resorting to SIGKILL"
lingering
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-3.slice -p Delegate=yes -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman --cgroups=split run -d --name lingering docker.io/library/redis:7-alpine sleep 3600; echo started'; echo "rc=$?"
Error: unknown flag: --cgroups
started
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'grep -l "^sleep" /proc/[0-9]*/comm; podman ps --format "{{.Names}} {{.Status}}"'
$ sudo systemd-cgls -M mill-e1 --no-pager
CGroup /machine.slice/mill-e1.scope:
-.slice
└─payload
  ├─user.slice
  │ └─user-1000.slice
  │   └─user@1000.service …
  │     ├─session.slice
  │     │ └─dbus-broker.service
  │     │   ├─2140430 /usr/bin/dbus-broker-launch --scope user
  │     │   └─2140431 dbus-broker --log 10 --controller 9 --machine-id f94084a2…
  │     ├─user.slice
  │     │ └─podman-pause-5d022444.scope
  │     │   └─2140415 catatonit -P
  │     └─init.scope
  │       ├─2140312 /usr/lib/systemd/systemd --user
  │       └─2140314 (sd-pam)…
  ├─init.scope
  │ └─2140195 /usr/lib/systemd/systemd
  └─system.slice
    ├─dbus-broker.service
    │ ├─2140294 /usr/bin/dbus-broker-launch --scope system --audit
    │ └─2140305 dbus-broker --log 10 --controller 9 --machine-id f94084a22f374f…
    ├─systemd-homed.service
    │ └─2140292 /usr/lib/systemd/systemd-homed
    ├─systemd-journald.service
    │ └─2140259 /usr/lib/systemd/systemd-journald
    ├─systemd-userdbd.service
    │ ├─2140266 /usr/lib/systemd/systemd-userdbd
    │ ├─2140309 systemd-userwork: waiting...
    │ ├─2140412 systemd-userwork: waiting...
    │ └─2140449 systemd-userwork: waiting...
    ├─console-getty.service
    │ └─2140308 /usr/sbin/agetty --noreset --noclear --issue-file=/etc/issue:/e…
    ├─systemd-oomd.service
    │ └─2140279 /usr/lib/systemd/systemd-oomd
    ├─systemd-resolved.service
    │ └─2140281 /usr/lib/systemd/systemd-resolved
    └─systemd-logind.service
      └─2140293 /usr/lib/systemd/systemd-logind
```

`lingering` survived the slice stop. `--cgroups` is a `podman run` flag, not a
global one, hence the third attempt.

### Third attempt: `podman run --cgroups=split` under a delegated exec

`--cgroups=split` keeps the container in the calling process's cgroup, split
into `runtime` (conmon, pasta, the shell's children) and `libpod-payload-<id>`
(the container). For `agent` to create those sub-cgroups the exec unit needs
`Delegate=yes`. The tree while step 3 was live, and the state once the exec
ended:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
lingering
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-3.slice -p Delegate=yes -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering docker.io/library/redis:7-alpine sleep 3600; echo started; sleep 8' & sleep 5; sudo systemd-cgls -M mill-e1 --no-pager /payload/mill.slice; wait; echo "rc=$?"
682918182ba7d1f8edc3605c78cddcf7bf2a7ec4381c3cf2c865feacd0a1d6a1
started
CGroup /machine.slice/mill-e1.scope/payload/mill.slice:
└─mill-step.slice
  └─mill-step-3.slice
    └─run-p2148982-i186569782.service …
      ├─libpod-payload-682918182ba7d1f8edc3605c78cddcf7bf2a7ec4381c3cf2c865feacd0a1d6a1
      │ └─2149035 sleep 3600
      └─runtime
        ├─2148985 sleep 8
        ├─2148986 sleep 3600
        ├─2149027 /usr/bin/pasta --config-net --dns-forward 169.254.1.1 -t none…
        └─2149033 /usr/bin/conmon --api-version 1 -c 682918182ba7d1f8edc3605c78…
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'grep -l "^sleep" /proc/[0-9]*/comm; podman ps --format "{{.Names}} {{.Status}}"; ls /proc/$(podman inspect lingering --format "{{.State.Pid}}")/comm 2>&1'   # after the exec ended, before the slice stops
lingering Up About a minute
ls: cannot access '/proc/1054/comm': No such file or directory
$ sudo systemctl -M mill-e1 stop mill-step-3.slice; echo "rc=$?"
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-4.slice -- sh -c 'grep -l "^sleep" /proc/[0-9]*/comm; podman ps --format "{{.Names}} {{.Status}}"; echo done'
lingering Up About a minute
done
```

The container and the `nohup sleep` sat under
`mill.slice/mill-step.slice/mill-step-3.slice/run-….service`, and **the end of
the exec unit reaped them** — `control-group` kill covers the delegated
sub-cgroups — so the slice stop found nothing left. Note `podman ps` still says
`Up About a minute` for a container whose pid does not exist: conmon was killed
before it could write state, so podman's view is stale after a cgroup kill.

To see the *slice* rather than the exec unit do the reaping, an exec with
`KillMode=process` leaves its children behind:

```
# to see the slice, not the exec unit, do the reaping: KillMode=process on the exec lets what it spawned outlive it
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
lingering
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-5.slice -p Delegate=yes -p KillMode=process -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering docker.io/library/redis:7-alpine sleep 3600; echo started'; echo "rc=$?"
bb10118175b8b920f896019fc384720498a8f573e1f2afa90b293a7c7fec0d76
started
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'for c in /proc/[0-9]*/comm; do grep -q "^sleep" $c && echo "$c $(tr -d "
" < $c) $(cat $(dirname $c)/cmdline | tr "\0" " ")"; done; podman ps --format "{{.Names}} {{.Status}}"; ls /proc/$(podman inspect lingering --format "{{.State.Pid}}")/comm'
/proc/1215/comm sleep sleep 3600 
/proc/1263/comm sleep sleep 3600 
lingering Up Less than a second
/proc/1263/comm
$ sudo systemd-cgls -M mill-e1 --no-pager /payload/mill.slice
CGroup /machine.slice/mill-e1.scope/payload/mill.slice:
└─mill-step.slice
  └─mill-step-5.slice
    └─run-p2153669-i186555368.service …
      ├─libpod-payload-bb10118175b8b920f896019fc384720498a8f573e1f2afa90b293a7c7fec0d76
      │ └─2153721 sleep 3600
      └─runtime
        ├─2153673 sleep 3600
        ├─2153713 /usr/bin/pasta --config-net --dns-forward 169.254.1.1 -t none…
        └─2153719 /usr/bin/conmon --api-version 1 -c bb10118175b8b920f896019fc3…
$ sudo systemctl -M mill-e1 stop mill-step-5.slice; echo "rc=$?"; sudo systemctl -M mill-e1 list-units --no-pager --no-legend "mill-step*"
rc=0
  mill-step-2.slice loaded active active Slice /mill/step/2
  mill-step-4.slice loaded active active Slice /mill/step/4
  mill-step.slice   loaded active active Slice /mill/step
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-6.slice -- sh -c 'for c in /proc/[0-9]*/comm; do grep -q "^sleep" $c && echo "$c"; done; podman ps --format "{{.Names}} {{.Status}}"; ls /proc/$(podman inspect lingering --format "{{.State.Pid}}")/comm; echo done'
/proc/1215/comm
/proc/1263/comm
lingering Up Less than a second
/proc/1263/comm
done
$ sudo systemd-cgls -M mill-e1 --no-pager /payload/mill.slice
CGroup /machine.slice/mill-e1.scope/payload/mill.slice:
└─mill-step.slice
  └─mill-step-5.slice
    └─run-p2153669-i186555368.service …
      ├─libpod-payload-bb10118175b8b920f896019fc384720498a8f573e1f2afa90b293a7c7fec0d76
      │ └─2153721 sleep 3600
      └─runtime
        ├─2153673 sleep 3600
        ├─2153713 /usr/bin/pasta --config-net --dns-forward 169.254.1.1 -t none…
        └─2153719 /usr/bin/conmon --api-version 1 -c bb10118175b8b920f896019fc3…
```

`systemctl stop mill-step-5.slice` returned 0, the slice unit is gone from
`list-units`, and every process is still there: stopping a slice stops the
units in it, and the orphans belong to a unit that had already ended and been
collected. The cgroup directory survives with them in it, and cgroup v2's
`cgroup.kill` on it is the reap:

```
# the slice unit is gone but its cgroup and the orphans remain; cgroup v2 cgroup.kill on the slice directory is the reap
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'ls /sys/fs/cgroup/mill.slice/mill-step.slice/; cat /sys/fs/cgroup/mill.slice/mill-step.slice/mill-step-5.slice/cgroup.procs; find /sys/fs/cgroup/mill.slice/mill-step.slice/mill-step-5.slice -name cgroup.procs -exec cat {} +'
cgroup.controllers
cgroup.events
cgroup.freeze
cgroup.kill
cgroup.max.depth
cgroup.max.descendants
cgroup.pressure
cgroup.procs
cgroup.stat
cgroup.stat.local
cgroup.subtree_control
cgroup.threads
cgroup.type
cpu.idle
cpu.max
cpu.max.burst
cpu.pressure
cpu.stat
cpu.stat.local
cpu.uclamp.max
cpu.uclamp.min
cpu.weight
cpu.weight.nice
cpuset.cpus
cpuset.cpus.effective
cpuset.cpus.exclusive
cpuset.cpus.exclusive.effective
cpuset.cpus.partition
cpuset.mems
cpuset.mems.effective
io.bfq.weight
io.latency
io.max
io.pressure
io.prio.class
io.stat
io.weight
irq.pressure
memory.current
memory.events
memory.events.local
memory.high
memory.low
memory.max
memory.min
memory.numa_stat
memory.oom.group
memory.peak
memory.pressure
memory.reclaim
memory.stat
memory.swap.current
memory.swap.events
memory.swap.high
memory.swap.max
memory.swap.peak
memory.zswap.current
memory.zswap.max
memory.zswap.writeback
mill-step-2.slice
mill-step-4.slice
mill-step-5.slice
mill-step-6.slice
pids.current
pids.events
pids.events.local
pids.max
pids.peak
1263
1215
1255
1261
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'echo 1 > /sys/fs/cgroup/mill.slice/mill-step.slice/mill-step-5.slice/cgroup.kill; echo "cgroup.kill rc=$?"; sleep 1; find /sys/fs/cgroup/mill.slice/mill-step.slice/mill-step-5.slice -name cgroup.procs -exec cat {} + | wc -l; ls /sys/fs/cgroup/mill.slice/mill-step.slice/'
cgroup.kill rc=0
find: ‘/sys/fs/cgroup/mill.slice/mill-step.slice/mill-step-5.slice’: No such file or directory
0
cgroup.controllers
cgroup.events
cgroup.freeze
cgroup.kill
cgroup.max.depth
cgroup.max.descendants
cgroup.pressure
cgroup.procs
cgroup.stat
cgroup.stat.local
cgroup.subtree_control
cgroup.threads
cgroup.type
cpu.pressure
cpu.stat
cpu.stat.local
io.pressure
irq.pressure
memory.current
memory.events
memory.events.local
memory.high
memory.low
memory.max
memory.min
memory.numa_stat
memory.oom.group
memory.peak
memory.pressure
memory.reclaim
memory.stat
memory.swap.current
memory.swap.events
memory.swap.high
memory.swap.max
memory.swap.peak
memory.zswap.current
memory.zswap.max
memory.zswap.writeback
mill-step-2.slice
mill-step-4.slice
mill-step-6.slice
pids.current
pids.events
pids.events.local
pids.max
pids.peak
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-7.slice -- sh -c 'for c in /proc/[0-9]*/comm; do grep -q "^sleep" $c && echo "$c"; done; podman ps --format "{{.Names}} {{.Status}}"; echo done'
lingering Up 26 seconds
done
$ sudo systemd-cgls -M mill-e1 --no-pager /payload/mill.slice
CGroup /machine.slice/mill-e1.scope/payload/mill.slice:
```

### What the exec unit's own kill costs

Writing the executable spec for this scenario found one more thing. The
step's exec, with a container whose pid 1 (`sleep`) ignores SIGTERM, took
92 s to return and returned 1 although `sh` exited 0:

```
$ time sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -p Delegate=yes -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering docker.io/library/alpine sleep 3600 && sleep 2'; echo "rc=$?"
daa2ebd0831a8f4c56255f0eb70ed1f6753d389c8ece89755d32ee54159243c1
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.01s user 0.00s system 0% cpu 1:32.41 total
rc=1
```

The unit's `control-group` kill sends SIGTERM, waits `TimeoutStopSec` (90 s
by default), then SIGKILLs, and the timeout makes the unit's result
`timeout`, which `--wait` reports as 1 in place of the main process's status.
Three properties tried:

```
# the exec unit waits TimeoutStopSec (90 s) for a container whose pid 1 ignores SIGTERM, then SIGKILLs it and reports failure
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
lingering
$ time sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -p Delegate=yes -p TimeoutStopSec=3s -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering docker.io/library/alpine sleep 3600 && sleep 2'; echo "rc=$?"
834ea7d40424a875fa85912feeb85af1a4e5ae260c0f2a650e2e23d7ac50dc2b
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.01s user 0.00s system 0% cpu 8.497 total
rc=1
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
lingering
$ time sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -p Delegate=yes -p KillSignal=SIGKILL -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering docker.io/library/alpine sleep 3600 && sleep 2'; echo "rc=$?"
5f011ef28616e8032603baf20910267a8c405bd4facaef5e6648a4666c953164
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.00s user 0.00s system 0% cpu 2.099 total
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'for c in /proc/[0-9]*/comm; do grep -q "^sleep" $c && echo "$c"; done; echo checked'
checked
```

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
lingering
$ time sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -p Delegate=yes -p KillMode=mixed -- sh -c 'nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering docker.io/library/alpine sleep 3600 && sleep 2'; echo "rc=$?"
db41d665586bb9bf7ec7e88eba554f4ef75298bc78ced018481c3ebe194f9d4e
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.01s user 0.01s system 0% cpu 2.120 total
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- podman rm -f lingering
lingering
# and a main process that exits non-zero, under KillSignal=SIGKILL: is the status still the main process's?
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Slice=mill-step-1.slice -p Delegate=yes -p KillSignal=SIGKILL -- sh -c 'podman run --cgroups=split -d --name lingering docker.io/library/alpine sleep 3600 >/dev/null && exit 42'; echo "rc=$?"
rc=42
```

`KillMode=mixed` on the exec unit — SIGTERM to the main process, SIGKILL to
everything else it left — returns in 2 s with the main process's own status,
and the leftovers are gone.

**Verdict: E3 passes with `--cgroups=split` — `docs/sandbox.md` §Step boundary needs it, and the exec unit needs `Delegate=yes` for `agent` to use it. Without it the container lands under `user@1000.service` and outlives the step. With it, containers and children sit under the step slice, and the exec unit's own `control-group` kill reaps them when it ends; in that configuration nothing is left for `systemctl stop <slice>` to do. The exec unit needs `KillMode=mixed`, or a step that leaves a container behind costs `TimeoutStopSec` and returns the wrong status. `systemctl stop` on a slice does not kill processes whose unit has already ended; `mill#62` should follow the stop with `cgroup.kill` on the slice's directory if it still exists, and must not read `podman ps` as evidence of anything after a kill.**

---

## E4 — The workspace bind is the same uid on both sides

### First attempt, as written

```
$ sudo mkdir -p /var/lib/mill/runs/e4/workspace && sudo chown 1000:1000 /var/lib/mill/runs/e4/workspace; ls -ldn /var/lib/mill/runs/e4/workspace
drwxr-xr-x. 1 1000 1000 0 Sep 11 00:08 /var/lib/mill/runs/e4/workspace
$ sudo systemctl stop mill-run-e1
$ t0=$(date +%s.%N); sudo systemd-run --unit=mill-run-e1 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false --bind=/var/lib/mill/runs/e4/workspace:/work; until sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.1; done; sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; echo "ready after $(echo "$(date +%s.%N) - $t0" | bc)s"
Running as unit: mill-run-e1.service; invocation ID: 180e912ec589495c8bc23ba0986f82fc
running
ready after .291445288s
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -- sh -c 'git init -q . && git -c user.name=e4 -c user.email=e4@example.invalid commit -q --allow-empty -m e4 && git rev-parse HEAD'; echo "rc=$?"
/work/.git: Permission denied
rc=1
$ sudo -u '#1000' git -C /var/lib/mill/runs/e4/workspace rev-parse HEAD
fatal: not a git repository (or any parent up to mount point /var/lib)
Stopping at filesystem boundary (GIT_DISCOVERY_ACROSS_FILESYSTEM not set).
$ stat -c '%u %g %U' /var/lib/mill/runs/e4/workspace/.git/HEAD /var/lib/mill/runs/e4/workspace
stat: cannot statx '/var/lib/mill/runs/e4/workspace/.git/HEAD': No such file or directory
1000 1000 ivy
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'findmnt -o TARGET,SOURCE,FSTYPE,OPTIONS /work; stat -c "%u %g" /work /work/.git/HEAD; grep " /work " /proc/self/mountinfo'
TARGET SOURCE                                     FSTYPE OPTIONS
/work  /dev/mapper/mill[/@mill/runs/e4/workspace] btrfs  rw,noatime,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill
65534 65534
stat: cannot statx '/work/.git/HEAD': No such file or directory
4349 4303 0:349 /@mill/runs/e4/workspace /work rw,noatime shared:1943 master:1866 - btrfs /dev/mapper/mill rw,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill
$ sudo journalctl -u mill-run-e1 --no-pager -o cat | grep -i -E "idmap|ownership|uid" | head
Started mill-run-e1.service - [systemd-run] /usr/sbin/systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false.
Initializing machine ID from container UUID.
[  OK  ] Created slice user-1000.slice - User Slice of UID 1000.
         Starting user@1000.service - User Manager for UID 1000...
[  OK  ] Started user@1000.service - User Manager for UID 1000.
Stopping mill-run-e1.service - [systemd-run] /usr/sbin/systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false...
         Stopping user@1000.service - User Manager for UID 1000...
[  OK  ] Stopped user@1000.service - User Manager for UID 1000.
[  OK  ] Removed slice user-1000.slice - User Slice of UID 1000.
Stopped mill-run-e1.service - [systemd-run] /usr/sbin/systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false.
$ ls -Z /var/lib/mill/runs/e4/workspace/.git/HEAD; sudo ausearch -m avc -ts 23:51:00
ls: cannot access '/var/lib/mill/runs/e4/workspace/.git/HEAD': No such file or directory
<no matches>
```

`/work` is uid 65534 inside — `nobody`, the overflow uid for an unmapped host
uid — and `git init` is refused. The bind is not idmapped:
`--private-users-ownership=auto` applies to the root directory (its mount is
`rw,noatime,idmapped`, below), not to `--bind=` mounts. `docs/sandbox.md`
§Filesystem says otherwise and is wrong on this point.

### Second attempt: the `idmap` bind option

```
# --private-users-ownership=auto covers the root tree, not binds: /work is nobody inside. Retry with the idmap bind option
$ sudo systemctl stop mill-run-e1
$ sudo systemd-run --unit=mill-run-e1 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false --bind=/var/lib/mill/runs/e4/workspace:/work:idmap; until sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.1; done
Running as unit: mill-run-e1.service; invocation ID: 0a39bc1f064747eaa9fac46907794151
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -- sh -c 'git init -q . && git -c user.name=e4 -c user.email=e4@example.invalid commit -q --allow-empty -m e4 && git rev-parse HEAD'; echo "rc=$?"
3e8d37ba5f855c6715338e57d41e16df8a780c56
rc=0
$ sudo -u '#1000' git -C /var/lib/mill/runs/e4/workspace rev-parse HEAD
3e8d37ba5f855c6715338e57d41e16df8a780c56
$ stat -c '%u' /var/lib/mill/runs/e4/workspace/.git/HEAD
1000
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'stat -c "%u %g %n" /work /work/.git/HEAD; grep " /work " /proc/self/mountinfo; cat /proc/self/uid_map'
1000 1000 /work
1000 1000 /work/.git/HEAD
4306 3931 0:349 /@mill/runs/e4/workspace /work rw,noatime,idmapped shared:1943 master:1866 - btrfs /dev/mapper/mill rw,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill
         0  946929664      65536
$ sudo findmnt -o TARGET,OPTIONS,PROPAGATION -N $(sudo machinectl show mill-e1 -p Leader --value) /work; sudo cat /proc/$(sudo machinectl show mill-e1 -p Leader --value)/mountinfo | grep " /work "
TARGET OPTIONS
/work  rw,noatime,idmapped,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill
4306 3931 0:349 /@mill/runs/e4/workspace /work rw,noatime,idmapped shared:1943 master:1866 - btrfs /dev/mapper/mill rw,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=256,subvol=/@mill
$ ls -Zn /var/lib/mill/runs/e4/workspace/.git/HEAD; sudo ausearch -m avc -ts 23:51:00
-rw-r--r--. 1 1000 1000 system_u:object_r:var_lib_t:s0 23 Sep 11 00:08 /var/lib/mill/runs/e4/workspace/.git/HEAD
<no matches>
```

The same SHA `3e8d37ba5f855c6715338e57d41e16df8a780c56` from both sides; `HEAD`
is owned by `1000` on the host, not `946929664 + 1000`; the mount inside is
`rw,noatime,idmapped`; the file on the host carries `var_lib_t`, and no AVC.
The root tree, for comparison, and what `ownership=auto` chose for it:

```
$ sudo cat /proc/$(sudo machinectl show mill-e1 -p Leader --value)/mountinfo | grep -E " / |/@machines" | head -3   # the ephemeral root: what ownership=auto chose
3931 3930 0:349 /@machines/.#machine.mill-image-spike48571f75fcc633c7 / rw,noatime,idmapped shared:1813 - btrfs /dev/mapper/mill rw,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=272,subvol=/@machines/.#machine.mill-image-spike48571f75fcc633c7
3932 3931 0:163 / /tmp rw,nosuid,nodev shared:1814 - tmpfs tmpfs rw,seclabel,size=3197924k,nr_inodes=409600,uid=946929664,gid=946929664,inode64,usrquota
4273 3931 0:173 / /sys ro,nosuid,nodev,noexec,relatime shared:1865 - tmpfs tmpfs rw,seclabel,size=4096k,nr_inodes=1024,mode=555,uid=946929664,gid=946929664,inode64
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'stat -c "%u %g %n" / /usr/bin/podman /home/agent; ls -Zd / /usr/bin/podman /home/agent'
0 0 /
0 0 /usr/bin/podman
1000 1000 /home/agent
                               ? /
system_u:object_r:unlabeled_t:s0 /home/agent
system_u:object_r:unlabeled_t:s0 /usr/bin/podman
$ sudo ls -ldn /var/lib/machines/mill-image-spike/usr/bin/podman /var/lib/machines/mill-image-spike/home/agent   # the image on the host, untouched by the boot
drwx------. 1 1000 1000       90 Sep 10 23:49 /var/lib/machines/mill-image-spike/home/agent
-rwxr-xr-x. 1    0    0 45220848 Jun 25 17:00 /var/lib/machines/mill-image-spike/usr/bin/podman
```

**Verdict: E4 passes with `--bind=<host>:/work:idmap`. The same SHA from both sides and owner `1000` on the host. `--private-users-ownership=auto` chose an idmapped mount for the root (`idmapped` in its mount options) but does not touch binds; every bind in §Provision — workspace, socket dir, skill dir, caches — needs the `:idmap` option, or `:rootidmap` where only root's files matter. No AVC on `/work`.**

---

## Teardown

```
$ sudo systemctl stop mill-run-e1 mill-machine@mill-e0.service 2>/dev/null; echo "rc=$?"
rc=0
$ sudo rm -rf /var/lib/mill/runs/e4
$ sudo rm -f /etc/polkit-1/rules.d/50-mill-machine-spike.rules
$ machinectl list --no-pager; ls -l /etc/polkit-1/rules.d/50-mill-machine-spike.rules; sudo btrfs subvolume list /var/lib/machines; ls -l /etc/systemd/system/mill-machine@.service; ls /var/lib/mill/runs 2>&1
No machines.
ls: cannot access '/etc/polkit-1/rules.d/50-mill-machine-spike.rules': Permission denied
ID 256 gen 63 top level 5 path @mill
ID 257 gen 65 top level 5 path @machines
ID 265 gen 35 top level 257 path mill-image-spike
-rw-r--r--. 1 root root 259 Sep 10 23:49 /etc/systemd/system/mill-machine@.service
$ systemctl start mill-machine@mill-e0.service; echo "start without the rule rc=$? (must be refused now)"
Failed to start mill-machine@mill-e0.service: Access denied as the requested operation requires interactive authentication. However, interactive authentication has not been enabled by the calling program.
See system logs and 'systemctl status mill-machine@mill-e0.service' for details.
start without the rule rc=1 (must be refused now)
```

`ls` as the operator cannot read `/etc/polkit-1/rules.d/`; as root:

```
$ sudo ls -l /etc/polkit-1/rules.d/50-mill-machine-spike.rules
ls: cannot access '/etc/polkit-1/rules.d/50-mill-machine-spike.rules': No such file or directory
```

The `runs` directory that `mkdir -p` created under `/var/lib/mill` was removed
too. **`/var/lib/machines/mill-image-spike` (the subvolume, rebuilt from the
committed script) and `/etc/systemd/system/mill-machine@.service` remain** for
the next four spike nodes; the last of them removes them.

---

## What the design takes from this

- **§Privilege.** The polkit template works for start and stop and refuses
  everything else; it does not give the operator an exec. `systemd-run -M`
  and `systemctl -M` enter the machine's namespaces themselves and want
  `CAP_SYS_ADMIN`; `machinectl shell` and machined's varlink `Open` are pty
  channels with no exit status. The shape is therefore the fallback the
  section already names: a root-owned helper the worker reaches over a socket
  or a unit, carrying exec (argv, environment, stdin/stdout/stderr, exit
  status, slice) and the slice stop, with start and stop either left on the
  polkit template or made the helper's other two verbs. The worker still never
  holds root.
- **§Destroy.** `systemctl stop mill-machine@<id>` shuts the machine down in
  a quarter of a second once it has booted, but a stop that lands before the
  container's init has its signal handlers is lost, the unit times out after
  90 s, and the payload survives in `mill-<id>.scope`, which the template's
  `KillMode=mixed` does not cover. Destroy is `systemctl stop` with a bound,
  then `machinectl terminate`; the polkit rule already admits the latter.
- **§Provision.** `State=running` precedes the machine's bus by ~200 ms;
  readiness is "an exec connects and `is-system-running --wait` returns", with
  connect failures retried. Each bind needs `:idmap`. Before the first exec as
  `agent`, mount a pristine `proc` (`mount -t proc -o nosuid,nodev,noexec proc /proc`)
  as the machine's root — from a unit in the image or from the template.
- **§Images / `mill#59`.** After `podman export`, `setcap cap_setuid=ep
  newuidmap` and `cap_setgid=ep newgidmap`; give `agent` subordinate ranges
  inside `[1, 65536)`; the base image's registry needs a `policy.json` entry
  (or a Harbor proxy project) on the build host; put `procps-ng` in the base
  package list if anything is going to `pgrep`.
- **§Exec.** `-p Delegate=yes` on every exec unit, so `agent`'s podman can
  create sub-cgroups under it, and `-p KillMode=mixed`, so what the exec
  leaves behind is SIGKILLed at once rather than after `TimeoutStopSec`, and
  the status `--wait` returns stays the main process's.
- **§Step boundary.** `--cgroups=split` is required on every `podman run`.
  Whether the image can pin it (`containers.conf`'s `cgroups=` documents
  `enabled`, `disabled` and `no-conmon`, not `split`) was not tried; until it
  is, `mill#62` passes it per invocation and the session's skill says so. The
  exec unit's
  `control-group` kill is what reaps in practice; `systemctl stop <slice>`
  then `cgroup.kill` on the slice directory if it still exists is the belt
  and braces. `podman ps` is not evidence after a kill.
- **§Filesystem.** "`--private-users-ownership=auto` idmaps the bind" is
  false; the bind option `idmap` does it.
- **§SELinux.** Nothing was denied across five experiments, with the image
  tree labelled `unlabeled_t` and the workspace `var_lib_t`: no
  `--selinux-context=`, no `--selinux-apifs-context=`, no policy module.
  `homestead#35` has an empty list to start from; the nested and network
  spikes may add to it.
- **nspawn flags the machine needed beyond the design's invocation:** `:idmap`
  on each `--bind=`. Nothing else — no `--system-call-filter=`,
  `--capability=` or SELinux flag.
- **Host facts the design did not have:** NetworkManager, not networkd,
  manages `core`'s interfaces, so `--network-veth` alone yields no address
  (the network spike's problem); Harbor was down for this session; the host's
  `policy.json` rejects registries it does not list.
