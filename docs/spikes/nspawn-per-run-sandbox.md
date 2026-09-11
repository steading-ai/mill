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
is E0's result. E14, the credential gateway, is the last section: a session
in a machine with an empty `~/.claude` answers through
`spikes/nspawn/gateway.py` on the host. E5, E6, E11 and E12 follow it:
egress through squid behind an nftables table (`spikes/nspawn/squid.conf`,
`spikes/nspawn/egress.nft`), the db suite inside a machine, pulls through
Harbor's proxy cache, and the `dnf` shim (`spikes/nspawn/dnf`). E9 and
E10 close the file: a cache lineage restored and promoted by snapshot, and a
derived image built under a lock by `spikes/nspawn/build-derived.sh`, booted,
and failed on purpose; their four scenarios are `spikes/nspawn/cache_test.go`.

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
| E14 | pass | `"result":"ok"` through the gateway with no `.credentials.json` in the machine; the upstream took the swapped bearer with the CLI's own headers and nothing added; tool search is off by default behind a non-Anthropic base URL |
| E5 | pass | `200` through squid with `CN=example.com`, `rc=28` without it; the table needs a DNS masquerade and the veth admitted to firewalld's `trusted` zone |
| E6 | pass | 231 db specs pass inside on `/run/user/1000/podman/podman.sock`, ryuk on; 43.9 s cold / 20.5 s warm against 19.2 s on the host; the proxy must reach `agent`'s user manager or podman cannot pull |
| E11 | pass | every `CONNECT` of a pull is to Harbor, none to an upstream registry; 2.4 s from a fresh machine; `RepoDigests` keeps the `docker.io` name by podman's design |
| E12 | pass | `dnf install -y vips-devel` as `agent` ends in the three-line hint; `sudo dnf remove` is `a password is required`; `--setopt=proxy=` goes after `install` for the sudoers pattern |
| E9 | pass | restore 1 ms, promote 52 ms for a 2.8G / 3.0G lineage; the run's `-tags e9` link output is in gen-2 and not gen-1; gen-1 deleted under the live run without effect; no `sudo` for create/snapshot/delete, but a read-only snapshot must be made writable before its owner can delete it, and `subvolume sync` needs root and is not what moves `df` — `filesystem sync` is |
| E10 | pass | `rpm -q libpq-devel` inside the derived machine; the second builder slept in `locks_lock_inode` and found the image `present`; the missing package leaves `rc=1`, no `.building`, and `No match for argument: no-such-package-e10`; build 4.3 s, export 0.8 s; 1005M against the base's 1020M, nothing shared |
| E13 | **pass, five of five** | the inner boots `--private-users=identity` from a snapshot the outer takes of the read-only bind, with `/proc` and the host's `/sys` bound into it; `machinectl list` in the outer names `inner`, hello prints inside it, `ok` from the subvolume round-trip as `agent` (with E9's `ro false`), `200` through the host squid from the inner; a 65536-uid outer is enough; no AVCs at any level; inner boot 2.26 s |
| E7 | **fail, zero of four** | `systemd-nsresourced` is denied `read` on `nsfs_t` (`systemd_nsresourced_t`), and the unprivileged `managed` boot hangs in `sigsuspend` on it; no policy change was made |
| E8 | **fail** (boots, `-M` does not reach it) | `--directory` dies in the host initrd on `root=root`; a `systemd-repart` raw image boots with the host kernel and `rd.luks=0 rd.lvm=0` to a getty in 9.8 s; `-M` is ssh over vsock and the tree has no `sshd` |

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

---

## E14 — The gateway with a setup-token upstream

The last spike node: a session inside a machine, with an empty `~/.claude`,
answers through a reverse proxy on the host that swaps its step token for the
subscription's `claude setup-token` value. The proxy is
`spikes/nspawn/gateway.py` (standard library, 183 lines): it binds a host
address, forwards only `/v1/messages…` to `https://api.anthropic.com`,
replaces `Authorization` with `Bearer $CLAUDE_CODE_OAUTH_TOKEN`, passes every
other header through, streams the response body back in chunks, and logs the
inbound token and the outbound host per request. The subscription token was
read from `.env` into the proxy's environment and nowhere else; the log lines
below carry the step token only.

Run 2026-09-11 00:41 – 00:49 PDT on the same host as E0–E4. Claude Code CLI
`2.1.266`.

### Setup: the machine, the veth, the CLI

The machine boots as in E1, with one extra flag: the host's CLI binary bound
read-only. `curl -fsSL https://claude.ai/install.sh | sh` was not tried — the
machine has no route out (the network spike's squid is not in place), and the
host's `/usr/bin/claude` is a static-ish ELF the Fedora image runs unchanged.

```
$ sudo systemd-run --unit=mill-run-e1 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e1 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false --bind-ro=/usr/bin/claude:/usr/local/bin/claude
Running as unit: mill-run-e1.service; invocation ID: 79e6aa487192491086976d85b811ce57
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- systemctl is-system-running --wait
running
$ ip -br addr show ve-mill-e1
ve-mill-e1@if2   DOWN
```

`--network-veth` yields a link with no address on either side, as the host
facts above predicted, and the image has no `ip` (iproute is not in the
package list) but does have `systemd-networkd`. Addressed by hand — a
`/run/systemd/network` file inside, `ip addr add` outside — and the veth put
in firewalld's `trusted` zone for the session, because the host's default zone
is `drop`:

```
$ sudo ip addr add 10.99.14.1/24 dev ve-mill-e1 && sudo ip link set ve-mill-e1 up
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'mkdir -p /run/systemd/network && printf "[Match]\nName=host0\n\n[Network]\nAddress=10.99.14.2/24\nLinkLocalAddressing=no\n" > /run/systemd/network/10-host0.network && systemctl start systemd-networkd && sleep 1 && networkctl status host0 --no-pager | head -20'
● 2: host0
                   Link File: n/a
                Network File: /run/systemd/network/10-host0.network
                       State: routable (configured)
                Online state: online
                        Type: ether
                        Kind: veth
…
                     Address: 10.99.14.2
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- curl -s -o /dev/null -w 'from machine to host: %{http_code}\n' http://10.99.14.1:8080/
from machine to host: 000
$ sudo firewall-cmd --get-default-zone
drop
$ sudo firewall-cmd --zone=trusted --add-interface=ve-mill-e1
success
```

Port 8080 on the host is held by a rootless podman `pasta` (`ss -ltnp` shows
`*:8080 users:(("pasta.avx2",…))`), and binding `10.99.14.1:8080` beside a
wildcard listener is `Address already in use`, so the gateway ran on **8081**
instead of the work order's 8080. Started with the token in its environment
and its log in a file:

```
$ (set -a; . ./.env; set +a; GATEWAY_BIND=10.99.14.1:8081 GATEWAY_LOG=/tmp/spike-out/gateway.log nohup python3 spikes/nspawn/gateway.py >/tmp/spike-out/gateway.stdout 2>&1 &)
$ cat /tmp/spike-out/gateway.log
listening bind=10.99.14.1:8081 upstream=https://api.anthropic.com swap=yes
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- curl -s -o /dev/null -w 'from machine, /v1/models: %{http_code}\n' -H 'Authorization: Bearer probe' http://10.99.14.1:8081/v1/models
from machine, /v1/models: 404
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'claude --version; ls -a ~/.claude 2>&1'
2.1.266 (Claude Code)
ls: cannot access '/home/agent/.claude': No such file or directory
```

### The run

The work order's command, with the port changed:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent \
    -p Environment=ANTHROPIC_BASE_URL=http://10.99.14.1:8081 -p Environment=ANTHROPIC_AUTH_TOKEN=step-token-e14 \
    -- claude -p 'reply with the single word ok' --output-format json
{"duration_api_ms":1251,"stop_reason":"end_turn","session_id":"476ad135-9325-4971-bb03-defb6160e048","total_cost_usd":0.16426625,"usage":{"input_tokens":2,"cache_creation_input_tokens":26265,"cache_read_input_tokens":0,"output_tokens":4,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":26265},"inference_geo":"not_available","iterations":[],"speed":"standard"},"modelUsage":{"claude-opus-5[1m]":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":0,"cacheCreationInputTokens":26265,"webSearchRequests":0,"costUSD":0.16426625,"contextWindow":1000000,"maxOutputTokens":64000,"thinkingTokens":0,"canonicalModel":"claude-opus-5","provider":"firstParty","costBasis":"list"}},"permission_denials":[],"terminal_reason":"completed","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","subagent_stats":{"spawned":0,"requested":{"background":0,"foreground":0,"unset":0},"started_in_background":0,"max_depth":0,"spawned_by_subagents":0,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0},"refused":{"depth_limit":0,"concurrency_limit":0,"budget":0},"by_type":{}},"is_error":false,"num_turns":1,"subtype":"success","api_error_status":null,"result":"ok","ttft_ms":1283,"type":"result","duration_ms":1300,"uuid":"407772d4-7b59-4580-844e-1c427ed6dd7b","ttft_stream_ms":1279,"time_to_request_ms":48,"first_content_frame_ms":1279,"queued_turn_count":0}
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -- sh -c 'ls -a ~/.claude; test ! -e ~/.claude/.credentials.json && echo no-login-file'
.
..
backups
projects
sessions
telemetry
no-login-file
$ cat /tmp/spike-out/gateway.log
listening bind=10.99.14.1:8081 upstream=https://api.anthropic.com swap=yes
refuse method=GET path=/v1/models
request method=POST path=/v1/messages?beta=true inbound-token=step-token-e14 outbound-host=api.anthropic.com swap=yes headers=accept,accept-encoding,anthropic-beta,anthropic-dangerous-direct-browser-access,anthropic-version,authorization,connection,content-length,content-type,host,user-agent,x-app,x-claude-code-session-id,x-stainless-arch,x-stainless-lang,x-stainless-os,x-stainless-package-version,x-stainless-retry-count,x-stainless-runtime,x-stainless-runtime-version,x-stainless-timeout
request method=POST path=/v1/messages?beta=true inbound-token=step-token-e14 outbound-host=api.anthropic.com swap=yes headers=accept,accept-encoding,anthropic-beta,anthropic-dangerous-direct-browser-access,anthropic-version,authorization,connection,content-length,content-type,host,user-agent,x-app,x-claude-code-session-id,x-stainless-arch,x-stainless-lang,x-stainless-os,x-stainless-package-version,x-stainless-retry-count,x-stainless-runtime,x-stainless-runtime-version,x-stainless-timeout
response inbound-token=step-token-e14 outbound-host=api.anthropic.com status=200 bytes=623 content-type=text/event-stream; charset=utf-8
```

`"result":"ok"`, `"is_error":false`, `"provider":"firstParty"`; 1.3 s end to
end. `~/.claude` afterwards holds the CLI's own state — a transcript under
`projects/`, a telemetry spool — and no `.credentials.json`. **The upstream
accepted the swapped bearer unchanged**: a 200 with `text/event-stream`, no
header added by the proxy beyond the `Authorization` swap and the `Host`
rewrite.

Two requests, one response line: the CLI opens a second `/v1/messages` call in
parallel with the answer (its session-title request: the body dump in the
tool-search run below shows it as `1.json`, no tools, "Write the title …")
and closes that connection when the first one finishes. The first version of the proxy logged nothing for it and
printed a `BrokenPipeError` traceback to stderr; the committed version catches
the disconnect and logs `client-closed …` instead. `mill#57` must expect a
client that abandons streams.

#### The headers the CLI sent

Names, from the log line above; the values were captured without a second
subscription call by pointing a second gateway (`GATEWAY_SWAP=0
GATEWAY_UPSTREAM=http://127.0.0.1:18099`, a stub that answers 400) at a fake
upstream and running the same `claude -p` against it (`GATEWAY_DUMP` writes
the inbound headers, `Authorization` omitted):

```
Accept: application/json
Content-Type: application/json
User-Agent: claude-cli/2.1.266 (external, sdk-cli)
X-Claude-Code-Session-Id: c65c533b-38cb-4fc3-85ef-320585d715f3
X-Stainless-Arch: x64
X-Stainless-Lang: js
X-Stainless-OS: Linux
X-Stainless-Package-Version: 0.112.1
X-Stainless-Retry-Count: 0
X-Stainless-Runtime: node
X-Stainless-Runtime-Version: v26.3.0
X-Stainless-Timeout: 600
anthropic-beta: claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,effort-2025-11-24,structured-outputs-2025-12-15
anthropic-dangerous-direct-browser-access: true
anthropic-version: 2023-06-01
x-app: cli
Connection: keep-alive
Host: 10.99.14.1:8082
Accept-Encoding: gzip, deflate, br, zstd
Content-Length: 4085
```

Plus `Authorization: Bearer step-token-e14`, and the query string
`?beta=true`. In `ANTHROPIC_AUTH_TOKEN` mode the CLI sends **no
`oauth-2025-04-20`** beta and no `x-api-key`; the upstream took the
subscription bearer with exactly this `anthropic-beta` list. **Nothing beyond
the bearer was required.** The gateway rewrites `Host` (the CLI sends the
gateway's), drops the hop-by-hop `Connection`, and passes the rest through,
`Accept-Encoding` included — the upstream's `Content-Encoding` travels back
untouched.

The fallback (`CLAUDE_CODE_OAUTH_TOKEN` in the session's environment) was not
run: it is for the case where the swap is refused, and it was not.

### Tool search with `ANTHROPIC_BASE_URL` at a non-Anthropic host

One more `claude -p`, with a local MCP server. `/work/.mcp.json` names the
CLI's own `claude mcp serve` — the only stdio MCP server the image has —
and is passed with `--mcp-config` because a project `.mcp.json` sits at
"Pending approval (run `claude` to approve)" in a non-interactive session:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -- sh -c 'printf "{\"mcpServers\":{\"local\":{\"command\":\"claude\",\"args\":[\"mcp\",\"serve\"]}}}\n" > /work/.mcp.json; claude mcp list 2>&1'
Checking MCP server health…

local: claude mcp serve - ⏸ Pending approval (run `claude` to approve)
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work \
    -p Environment=ANTHROPIC_BASE_URL=http://10.99.14.1:8081 -p Environment=ANTHROPIC_AUTH_TOKEN=step-token-e14 \
    -- claude -p 'Answer with only a comma-separated list of the names of the tools you have from the MCP server named local, and nothing else.' --output-format json --mcp-config /work/.mcp.json --strict-mcp-config
{"result":"Agent, Bash, CronCreate, CronDelete, CronList, DesignSync, Edit, EnterWorktree, ExitWorktree, ListAgents, NotebookEdit, Read, ReportFindings, ScheduleWakeup, SendMessage, Skill, TaskOutput, TaskStop, WebFetch, WebSearch, Workflow, Write","is_error":false,"num_turns":1,…}
```

The request bodies the gateway dumped (`GATEWAY_DUMP`), reduced to their
`tools` arrays:

```
1.json model= claude-opus-5 stream= True ntools= 0          # the title request
2.json model= claude-opus-5 stream= True ntools= 44
  defer_loading on any: False | tool-search tool types: []
  mcp tools: ['mcp__local__Agent', 'mcp__local__Bash', … 22 in all … 'mcp__local__Write']
```

All 22 MCP tools go up **inline, in full, with no `defer_loading`, and no
tool-search tool** in the request; the model sees and lists them. The CLI's
own reason is in its binary:

```
$ strings -n 8 /usr/bin/claude | grep -F 'ToolSearch:optimistic'
[ToolSearch:optimistic] disabled: ANTHROPIC_BASE_URL=
 is not a first-party Anthropic host. Set ENABLE_TOOL_SEARCH=true (or auto / auto:N) if your proxy forwards tool_reference blocks.
```

So a base URL at the gateway turns tool search **off by default**: tools are
listed, not searched, and a session with many MCP servers pays for every
definition on every turn. The gateway forwards bodies byte for byte, so
`ENABLE_TOOL_SEARCH=true` (or `auto`) in the exec environment restores the
deferred shape; `mill#57` or `mill#62` sets it, and it is a workspace
decision like the proxy variables.

### The executable form

`spikes/nspawn/gateway_test.go` holds the three scenarios. The two that need
no machine run in `make test`: `/v1/models` is 404, and a request through the
gateway to a stand-in upstream is logged with the step token inbound and the
upstream host outbound while the stand-in sees the swapped bearer and the log
never contains it. The third boots a machine, addresses the veth, starts the
gateway with `CLAUDE_CODE_OAUTH_TOKEN` from the environment, and runs the
`claude -p`; it is `Label("live")` and was run once:

```
$ (set -a; . ./.env; set +a; MILL_SANDBOX_HOST=1 ginkgo -v --label-filter=live --focus='no credential' ./spikes/nspawn)
The credential gateway on a real machine The session answers with no credential in the machine [live, Serial]
  STEP: a machine whose agent has an empty ~/.claude @ 09/11/26 00:48:36.867
  STEP: the veth addressed on both sides and admitted by the host's firewall @ 09/11/26 00:48:37.157
  STEP: a gateway on the host holding the subscription token @ 09/11/26 00:48:37.424
  STEP: the session runs claude -p with the step token and the gateway as base URL @ 09/11/26 00:48:37.445
  STEP: the answer is ok and no .credentials.json exists in the machine @ 09/11/26 00:48:38.874
• [2.498 seconds]
Ran 1 of 7 Specs in 2.498 seconds
SUCCESS! -- 1 Passed | 0 Failed | 0 Pending | 6 Skipped
```

**Verdict: E14 passes. The CLI answers `ok` through the gateway with an empty `~/.claude` and no `.credentials.json` afterwards; the gateway log pairs `inbound-token=step-token-e14` with `outbound-host=api.anthropic.com` on the same request; the upstream accepted the swapped bearer with the CLI's own headers and nothing added. Decision for `mill#57`: the gateway with a swapped bearer, as §Credentials describes; the fallback is not needed.**

### Teardown

The last spike node removes the template unit and the image, and the two
`.#machine.mill-image-spike…` snapshots (plus their `.lck` files) that earlier
sessions left behind after a machine died without nspawn's cleanup:

```
$ sudo systemctl stop mill-run-e1 2>/dev/null; echo "rc=$?"
rc=5
$ sudo systemctl stop mill-machine@*.service 2>/dev/null; echo "rc=$?"
rc=1
$ sudo rm -f /etc/systemd/system/mill-machine@.service && sudo systemctl daemon-reload; echo "rc=$?"
rc=0
$ sudo btrfs subvolume delete /var/lib/machines/mill-image-spike
Delete subvolume 273 (no-commit): '/var/lib/machines/mill-image-spike'
Delete subvolume 282 (no-commit): '/var/lib/machines/.#machine.mill-image-spikee0b6c81c3f66d6e5'
Delete subvolume 284 (no-commit): '/var/lib/machines/.#machine.mill-image-spikedf0e8928e4e6ad47'
$ machinectl list --no-pager; ls -l /etc/systemd/system/mill-machine@.service 2>&1; sudo btrfs subvolume list /var/lib/machines; sudo ls -a /var/lib/machines
No machines.
ls: cannot access '/etc/systemd/system/mill-machine@.service': No such file or directory
ID 256 gen 145 top level 5 path @mill
ID 257 gen 154 top level 5 path @machines
.
..
$ sudo firewall-cmd --zone=trusted --list-interfaces

$ sudo ausearch -m avc -ts 00:40:00
<no matches>
```

`rc=5` and `rc=1` on the two stops are "no such unit": the machine had been
stopped after the run, and no `mill-machine@` instance existed. The veth was
taken out of the `trusted` zone before the machine stopped; the change was
runtime-only and never `--permanent`.

Fifteen minutes later the host held a new `/var/lib/machines/mill-image-spike`
(created 00:52:47, a different subvolume id) and a 1640-byte
`/etc/systemd/system/mill-machine@.service` describing itself as "mill:
sandbox machine for run %i", with `mill-machine@x.service` running: another
agent on this host, working `homestead#33`, installing the real ones. Those
are not the spike's and were left alone.

### What the design takes from E14

- **§Credentials stands as written.** The step token in
  `ANTHROPIC_AUTH_TOKEN`, the gateway at `ANTHROPIC_BASE_URL`, the bearer
  swapped for the setup-token: the CLI works with an empty `~/.claude`, and
  the upstream needs nothing beyond the swapped bearer. The fallback paragraph
  can go.
- **`mill#57`.** Forward every request header except `Host` and the
  hop-by-hop set, rewrite `Host`, swap `Authorization`; refuse paths outside
  `/v1/messages`; stream the body; tolerate a client that closes a stream
  midway (the CLI's parallel title request does) without treating it as an
  upstream failure; and set `ENABLE_TOOL_SEARCH=true` in the exec environment
  or accept that tool search is off for every session.
- **§Network / `mill#61`.** The host-side veth address is the worker's to
  assign (NetworkManager does not), the image needs `systemd-networkd`
  enabled with a `host0` `.network` file (or `iproute` for the helper), and
  firewalld's `drop` default zone must admit the veth or nothing on the host
  answers — squid included.
- **§Images / `mill#59`.** The host's `claude` binary runs unchanged in the
  image when bound in; installing it into the image at build time is the
  cleaner shape. The image's port for the gateway must be chosen against what
  the host already listens on: rootless podman's `pasta` held 8080 here.

---

## E5 — Egress is host-enforced

Run 2026-09-11 02:50 – 02:56 PDT on the same host as E0–E14. `squid-7.6-1.fc44`
was already installed from the first attempt at this node; `/etc/squid/squid.conf`
is `spikes/nspawn/squid.conf` byte for byte (Fedora's default minus the
`localnet` ACLs, `http_port 10.99.14.1:3128`, `cache deny all`), and the
nftables table is `spikes/nspawn/egress.nft`. The machine boots as E1 did,
through `/tmp/spike-out/boot.sh`, which runs exactly this and then the E2
`/proc` recipe and the veth addressing E14 settled on (`10.99.14.1` host side,
`10.99.14.2` inside, `DNS=1.1.1.1` in the machine's `.network`):

```
sudo systemd-run --unit=mill-run-$name --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-$name --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false "$@"
```

The image subvolume from the boot spike was still on the host (`ID 298 …
mill-image-spike`), so no rebuild. Homestead's `/etc/systemd/system/mill-machine@.service`
(1640 bytes, `Sep 11 01:03`) was left untouched.

### The table, and squid's one ordering constraint

```
$ sudo nft -f spikes/nspawn/egress.nft && sudo nft list table inet mill
table inet mill {
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "ve-mill*" udp dport 53 accept
		iifname "ve-mill*" tcp dport 53 accept
		iifname "ve-mill*" ct state established,related accept
		iifname "ve-mill*" drop
	}

	chain input {
		type filter hook input priority filter; policy accept;
		iifname "ve-mill*" tcp dport 3128 accept
		iifname "ve-mill*" udp dport 53 accept
		iifname "ve-mill*" ct state established,related accept
		iifname "ve-mill*" drop
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		iifname "ve-mill*" oifname != "ve-mill*" masquerade
	}
}
$ sudo systemctl start squid; systemctl is-active squid; sudo ss -ltnp | grep 3128
Job for squid.service failed because the control process exited with error code.
See "systemctl status squid.service" and "journalctl -xeu squid.service" for details.
failed
$ sudo tail /var/log/squid/cache.log
2026/09/11 02:50:39 kid1| ERROR: commBind Cannot bind socket FD 10 to 10.99.14.1:3128: (99) Cannot assign requested address
2026/09/11 02:50:39 kid1| FATAL: Unable to open HTTP Socket
```

The table is the work order's plus one `nat postrouting` chain: DNS is the
one thing forwarded, and `10.99.0.0/16` is private, so the query needs a
source rewrite to be answered. `ip_forward` was already `1` on the host
(libvirt). Squid binds its address at start and the veth does not exist until
a machine does, so the proxy cannot start before the first machine on this
configuration — `homestead#34` either binds a host address that always exists
(a dummy link, or `0.0.0.0:3128` with the `machines` ACL doing the work) or
orders the unit after the link. Booted, addressed, then squid:

```
$ /tmp/spike-out/boot.sh e1 14 --bind=$PWD:/work:idmap --bind-ro=/home/ivy/src/github.com/ivy/dagger:/opt/dagger:idmap --bind-ro=/tmp/spike-out/50-mill-proxy.conf:/etc/environment.d/50-mill-proxy.conf
Running as unit: mill-run-e1.service; invocation ID: 61017e58290843a48c5486845f55ae78
running
                       State: routable (configured)
                     Address: 10.99.14.2
$ ip -4 addr show ve-mill-e1
61: ve-mill-e1@if2: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default qlen 1000 link-netnsid 0
    inet 10.99.14.1/24 scope global ve-mill-e1
       valid_lft forever preferred_lft forever
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- networkctl status host0 --no-pager | grep -E "Address|Gateway|DNS"
                     Address: 10.99.14.2
                     Gateway: 10.99.14.1
                         DNS: 1.1.1.1
$ sudo systemctl start squid; systemctl is-active squid; sudo ss -ltnp | grep 3128
active
LISTEN 0      4096                    10.99.14.1:3128       0.0.0.0:*    users:(("squid",pid=366853,fd=10))
```

(The two extra binds are E6's: the checkout and the dagger checkout. The
`environment.d` file is E6's too — see there.) No capability was needed on
the nspawn invocation for the veth: `--network-veth` under `sudo` brings the
link up as it did in E1 and E14.

### firewalld sits in front of the table

The host's default zone is `drop`. With the veth in no zone, DNS through the
forward path answered but TCP to the host's own 3128 did not:

```
$ sudo firewall-cmd --get-zone-of-interface=ve-mill-e1
no zone
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- sh -c 'getent hosts example.com; echo "rc=$?"'
2606:4700:10::ac42:93f3 example.com
2606:4700:10::6814:179a example.com
rc=0
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p Environment=HTTPS_PROXY=http://10.99.14.1:3128 -- curl -sS --max-time 5 -o /dev/null -w '%{http_code}\n' https://example.com/; echo "rc=$?"
curl: (28) Connection timed out after 5002 milliseconds
000
rc=28
$ sudo firewall-cmd --zone=trusted --add-interface='ve-mill+'
success
```

An accept in `inet mill`'s input chain does not skip `inet firewalld`'s input
hook, so the link must be admitted there as well; the enforcement stays in
`inet mill`. `ve-mill+` is firewalld's glob form and covers every machine.
Runtime only, not `--permanent`.

### The three probes

```
$ P=http://10.99.14.1:3128
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p Environment=HTTPS_PROXY=$P -- curl -sS -o /dev/null -w '%{http_code}\n' https://example.com/
200
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p Environment=HTTPS_PROXY=$P -- sh -c 'curl -sSv https://example.com/ 2>&1 >/dev/null | grep -E "issuer|subject"'
*   subject: CN=example.com
*   issuer: C=US; O=SSL Corporation; CN=Cloudflare TLS Issuing ECC CA 3
*   subjectAltName: "example.com" matches cert's "example.com"
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- curl -sS --max-time 5 -o /dev/null https://example.com/; echo "rc=$?"
curl: (28) Connection timed out after 5002 milliseconds
rc=28
$ sudo cat /var/log/squid/access.log
1789120291.505     81 10.99.14.2 TCP_TUNNEL/200 6450 CONNECT example.com:443 - HIER_DIRECT/172.66.147.243 -
1789120291.554     21 10.99.14.2 TCP_TUNNEL/200 6449 CONNECT example.com:443 - HIER_DIRECT/172.66.147.243 -
```

`200`; the certificate is the origin's (`CN=example.com`, issued by
Cloudflare — squid did not intercept); without the variable the connect
hangs to the `--max-time` and `rc=28`. Each access-log line has the client
address, `CONNECT host:port`, the verdict and the bytes — a per-workflow
allowlist is an `acl … dstdomain` against exactly the field `example.com:443`
sits in.

### The shape of the drop

What else a machine can and cannot reach, to know what the table gives:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- curl -sS --max-time 3 -o /dev/null http://192.168.50.1/; echo "rc=$?"    # the LAN, direct
curl: (28) Connection timed out after 3002 milliseconds
rc=28
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- curl -sS --max-time 3 -o /dev/null http://10.99.14.1:8080/; echo "rc=$?"    # the host, a port other than 3128
curl: (28) Connection timed out after 3002 milliseconds
rc=28
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p Environment=HTTPS_PROXY=$P -- curl -sS --max-time 5 -o /dev/null -w '%{http_code}\n' https://192.168.50.1/; echo rc=$?    # the LAN, through the proxy
curl: (56) CONNECT tunnel failed, response 503
000
rc=56
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p Environment=HTTP_PROXY=$P -- curl -sS --max-time 40 -o /dev/null -w '%{http_code} %{time_total}\n' http://example.com/
curl: (28) Connection timed out after 40002 milliseconds
000 40.002459
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p Environment=http_proxy=$P -- curl -sS --max-time 5 -o /dev/null -w '%{http_code}\n' http://example.com/
200
$ sudo tail -2 /var/log/squid/access.log
1789120319.622      0 10.99.14.2 TCP_TUNNEL/503 0 CONNECT 192.168.50.1:443 - HIER_DIRECT/192.168.50.1 -
1789120387.454     35 10.99.14.2 TCP_MISS/200 936 GET http://example.com/ - HIER_DIRECT/104.20.23.154 text/html
```

The LAN and the host's other ports drop as the table says. The LAN *through*
the proxy is a squid `503`: the router refused squid's connect, not the
table — `squid.conf` has no rule against RFC 1918 destinations, and
`homestead#34` should add one, because the table cannot see what squid
connects to on the machine's behalf. (The committed `spikes/nspawn/squid.conf`
carries that rule — `acl lan dst 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16` and
`http_access deny lan` — added after this run and checked with
`squid -k parse` only; what ran is the file without those two lines.) And
`curl` ignores an uppercase
`HTTP_PROXY` (the httpoxy mitigation; only `http_proxy` is read for plain
`http://`) — the 40 s hang never reached squid — so §Network's "the variables
are set in every exec's environment" means both cases of all three names.
`HTTPS_PROXY` in either case works.

**Verdict: E5 passes. `200` through the proxy with the origin's certificate; `rc=28` without it. The table egress needs is the work order's plus a `nat postrouting` masquerade for the forwarded DNS, and the `ve-mill*` links admitted to firewalld's `trusted` zone — firewalld's `drop` default zone otherwise stops the proxy port before `inet mill` is consulted. Squid must start after the address it binds exists. Both cases of the proxy variables go in the environment.**

---

## E6 — The real test suite, inside

Same machine as E5, booted with the checkout at `/work` and the dagger
checkout at `/opt/dagger`, both `:idmap` (E4). The fixture in
`internal/testsupport/dagger.go` finds dagger's migrations and `dg` through
`MILL_DAGGER_REPO`, so that is set on the exec; `docs/sandbox.md` §Testing
already says the mill repo's machine gets the dagger checkout bound
read-only.

### The checkout is writable as `agent`

The work order's fallback (a copy owned by uid 1000) was not needed: the
operator is uid 1000 and `:idmap` makes the bind uid 1000 inside.

```
$ ls -ld $PWD; id -u
drwxr-xr-x. 17 ivy ivy 4096 Sep 11 02:53 /home/ivy/src/github.com/ivy/mill
1000
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -- sh -c 'id; ls -ld /work /work/.git /opt/dagger; touch /work/.e6-probe && echo writable && rm /work/.e6-probe; ls -l /run/user/1000/podman/podman.sock /run/podman/podman.sock; cat /etc/environment.d/50-mill-proxy.conf'
uid=1000(agent) gid=1000(agent) groups=1000(agent)
drwxr-xr-x. 16 agent agent 4096 Sep  8 02:05 /opt/dagger
drwxr-xr-x. 17 agent agent 4096 Sep 11 02:17 /work
drwxr-xr-x.  8 agent agent 4096 Sep 11 02:48 /work/.git
writable
ls: cannot access '/run/podman/podman.sock': Permission denied
srw-rw----. 1 agent agent 0 Sep 11 02:51 /run/user/1000/podman/podman.sock
HTTPS_PROXY=http://10.99.14.1:3128
HTTP_PROXY=http://10.99.14.1:3128
https_proxy=http://10.99.14.1:3128
http_proxy=http://10.99.14.1:3128
NO_PROXY=localhost,127.0.0.1
no_proxy=localhost,127.0.0.1
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -- ls -l /run/podman/podman.sock
srw-rw----. 1 root root 0 Sep 11 02:51 /run/podman/podman.sock
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=XDG_RUNTIME_DIR=/run/user/1000 -- sh -c 'systemctl --user show-environment | grep -i proxy'
HTTPS_PROXY=http://10.99.14.1:3128
HTTP_PROXY=http://10.99.14.1:3128
https_proxy=http://10.99.14.1:3128
http_proxy=http://10.99.14.1:3128
NO_PROXY=localhost,127.0.0.1
no_proxy=localhost,127.0.0.1
```

Both podman sockets exist: the system one at `/run/podman/podman.sock` is
`root:root 0660` and out of `agent`'s reach; `agent`'s own at
`/run/user/1000/podman/podman.sock` is there because the image lingers
`agent` and enables the user `podman.socket`. **That is the socket path**,
and it is what `useRootlessPodmanSocket` in `internal/testsupport` picks by
itself when `DOCKER_HOST` is unset.

### Why the user manager carries the proxy

The `environment.d` file bound in at boot is the first attempt's finding, and
the reason its E6 failed three times. Testcontainers does not pull the image;
it asks `podman.service` — `agent`'s user service, started by the user
manager from `socket` activation — to create the container, and the service
pulls with *its* environment, which has no proxy however many variables the
exec carries. The first attempt's `make test-db` inside spent 123 s per suite
on:

```
      6           msg: "start postgres container: create container: Error response from daemon: i/o timeout",
      1 Ran 0 of 107 Specs in 123.392 seconds
```

Dropping `/etc/environment.d/50-mill-proxy.conf` into the running machine and
restarting `user@1000.service` did not recover it (the restart failed in
`pam_systemd`'s `CreateSession` against logind); booting with the file bound
in did. §Network's "`HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` are set in every
exec's environment" is therefore not enough for podman: **the proxy has to be
in the user manager's environment too** — `environment.d` in the image with
the address filled at provision, or a drop-in on the user `podman.service`.
`NO_PROXY=localhost,127.0.0.1` is there so the published ports testcontainers
dials on the machine's loopback are not sent to squid.

### `mise install`, cold, through the proxy

```
$ P=http://10.99.14.1:3128
$ time sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -p Environment=HTTPS_PROXY=$P -p Environment=HTTP_PROXY=$P -- mise install 2>&1 | tail -15
mise ginkgo@2.32.1 go: downloading github.com/Masterminds/semver/v3 v3.4.0
mise ginkgo@2.32.1 go: downloading golang.org/x/sync v0.20.0
mise ginkgo@2.32.1 go: downloading golang.org/x/mod v0.35.0
mise python@3.14.7 Python 3.14.7
mise ✓ python@3.14.7              3.1s  cpython-3.14.7+20260901-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz
mise ✓ golangci-lint@2.13.2       3.8s  golangci-lint-2.13.2-linux-amd64.tar.gz
mise ✓ pnpm@10.20.0               4.0s  pnpm-linux-x64
mise ✓ hk@1.57.0                  2.9s  hk-x86_64-unknown-linux-gnu.tar.gz
mise █████████████░░░ 8/10 · 6.0s
  buf@1.72.0                 downloading  4.3s
  ginkgo@2.32.1              installing   2.1s
mise ✓ buf@1.72.0                 5.0s  buf-Linux-x86_64.tar.gz
mise ✓ ginkgo@2.32.1              4.8s
mise ████████████████ 10/10 · installed 10 tools in 8.7s
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.00s user 0.01s system 0% cpu 8.750 total
rc=0
```

Ten tools, 8.7 s, every fetch a `CONNECT` in squid's log (`github.com`,
`dl.google.com`, `nodejs.org`, `mise-versions.jdx.dev`,
`release-assets.githubusercontent.com`, `downloads.sqlc.dev`,
`storage.googleapis.com`, `tuf-repo-cdn.sigstore.dev` — the list an allowlist
for a toolchain step would start from).

### `make test-db`, as written, then through `mise exec`

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -p Environment=HTTPS_PROXY=$P -p Environment=HTTP_PROXY=$P -p Environment=DOCKER_HOST=unix:///run/user/1000/podman/podman.sock -- make test-db 2>&1 | head -5; echo rc=$?
ginkgo -r -p --randomize-all --fail-on-pending --keep-going --label-filter='db' ./...
make: ginkgo: No such file or directory
make: *** [Makefile:48: test-db] Error 127
rc=2
```

An exec is not a login shell, so nothing activates mise and the installed
tools are not on `PATH`. `mise exec --` in front of the command is the
answer; a preparatory step or the session's shell would have it from
`mise activate`. Cold — the machine's Go module cache, build cache and
container store are all empty:

```
$ date +%T; time sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -p Environment=HTTPS_PROXY=$P -p Environment=HTTP_PROXY=$P -p Environment=DOCKER_HOST=unix:///run/user/1000/podman/podman.sock -p Environment=MILL_DAGGER_REPO=/opt/dagger -- mise exec -- make test-db > /tmp/spike-out/e6-cold.log 2>&1; echo rc=$?; date +%T
02:54:00
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.01s user 0.00s system 0% cpu 43.875 total
rc=0
02:54:44
$ sed 's/\x1b\[[0-9;]*m//g' /tmp/spike-out/e6-cold.log | grep -E 'specs - 15 procs' | grep -v ' 0/'
[1789120440] cli - 65/107 specs - 15 procs … SUCCESS! 6.449760811s
[1789120440] dagger - 12/12 specs - 15 procs … SUCCESS! 2.656700233s
[1789120440] run - 73/84 specs - 15 procs … SUCCESS! 8.622844905s
[1789120440] server - 36/63 specs - 15 procs … SUCCESS! 1.896098607s
[1789120440] store - 41/53 specs - 15 procs … SUCCESS! 2.448982571s
[1789120440] testsupport - 4/4 specs - 15 procs … SUCCESS! 1.381165896s
$ tail -3 /tmp/spike-out/e6-cold.log
Ginkgo ran 29 suites in 43.779736194s
Test Suite Passed
$ grep -c 'go: downloading' /tmp/spike-out/e6-cold.log
259
```

(The 23 suites with no `db` spec ran and skipped; the per-suite lines are
elided to the six that carry the label. The `•`/`S` progress runs are cut
from the lines above.) Then the same command again on the same machine —
module cache, build cache and image store now warm — and the host, for the
comparison:

```
$ date +%T; time sudo systemd-run -M mill-e1 … -- mise exec -- make test-db > /tmp/spike-out/e6-warm.log 2>&1; echo rc=$?; date +%T
02:55:12
sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p   0.01s user 0.01s system 0% cpu 20.500 total
rc=0
02:55:32
$ tail -2 /tmp/spike-out/e6-warm.log
Ginkgo ran 29 suites in 20.366331241s
Test Suite Passed
$ time make test-db  # on the host, warm
Ginkgo ran 29 suites in 19.07033682s
Test Suite Passed
make test-db  35.57s user 15.08s system 264% cpu 19.164 total
```

| | wall clock | ginkgo's own |
|---|---|---|
| inside, cold | 43.9 s | 43.8 s |
| inside, warm | 20.5 s | 20.4 s |
| host, warm | 19.2 s | 19.1 s |

Inside is within 7 % of the host once warm. The cold run is 2.3× the host,
and the difference is the empty caches: 259 `go: downloading` lines through
the proxy (32 `CONNECT proxy.golang.org`), two image pulls through Harbor
(`postgres:18` for the fixture and `testcontainers/ryuk`), and a full Go
build with no cache. A Go module cache under §Caching would take the first
of those away.

### Ryuk, and what is left in the store

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=XDG_RUNTIME_DIR=/run/user/1000 -- sh -c 'podman ps -a; podman images'    # 32 s after the cold run
CONTAINER ID  IMAGE                                 COMMAND     CREATED         STATUS         PORTS                    NAMES
fabda3952b31  docker.io/testcontainers/ryuk:0.14.0  /bin/ryuk   32 seconds ago  Up 32 seconds  0.0.0.0:38113->8080/tcp  reaper_08506bc46bac892faaf0163c9748377af9b1332e6bb220ca898951d2d9e02043
REPOSITORY                     TAG         IMAGE ID      CREATED        SIZE
docker.io/library/postgres     <none>      a6638641707c  2 weeks ago    464 MB
docker.io/testcontainers/ryuk  0.14.0      9a5f93e8f930  11 months ago  2.41 MB
$ until [ "$(sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=XDG_RUNTIME_DIR=/run/user/1000 -- podman ps -aq | wc -l)" = 0 ]; do sleep 10; done; date +%T
03:00:37
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=XDG_RUNTIME_DIR=/run/user/1000 -- podman ps -a
CONTAINER ID  IMAGE       COMMAND     CREATED     STATUS      PORTS       NAMES
```

**Ryuk was not disabled.** `TESTCONTAINERS_RYUK_DISABLED` was never set;
`internal/testsupport` defaults `TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED=true`
and `RYUK_RECONNECTION_TIMEOUT=5m`, and a privileged container under rootless
podman under nspawn ran and reaped. The postgres containers were gone when the
suite ended; the reaper itself stays for the fixture's five-minute window
(`patientReaper`, so a spec sleeping past a TTL does not lose its database)
and `podman ps -a` was empty at 03:00:37, five minutes after the warm run's
last connection. A machine is destroyed at the end of its run, so the window
costs nothing there; a check step that counted containers right after the
suite would see the reaper and should not.

**Verdict: E6 passes. 231 db-labelled specs across six suites pass inside the machine; testcontainers starts PostgreSQL through `agent`'s `/run/user/1000/podman/podman.sock` and ryuk reaps it, undisabled; `podman ps -a` is empty once ryuk's window closes. Wall clock 43.9 s cold / 20.5 s warm against the host's 19.2 s; the cold gap is the empty module and build caches. Two things the design has to add: the proxy in `agent`'s user-manager environment (podman's service pulls, not the exec), and `mise exec` or an activated shell in front of any command that wants the toolchain.**

---

## E11 — Harbor proxy: image pulls inside the machine

Harbor answered this time (`/api/v2.0/health` → `200` at the start of the
session, against `502` for the whole of the boot spike). The image's
`registries.conf.d/harbor.conf` is the one `Containerfile.spike` writes and
needed no change:

```
[[registry]]
prefix = "docker.io"
location = "harbor.tailcbcc0e.ts.net/dockerhub-cache"

[[registry]]
prefix = "quay.io"
location = "harbor.tailcbcc0e.ts.net/quay-cache"
```

### First pull, on mill-e1

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=HTTPS_PROXY=$P -- sh -c 'time podman pull docker.io/library/postgres:16; podman image inspect --format "{{.RepoDigests}}" docker.io/library/postgres:16'
Trying to pull docker.io/library/postgres:16...
Getting image source signatures
Copying config sha256:80f4c7a5e91618546dce5b4fe60cf03b14c0f9efa7e40157278d122772ced8d2
Writing manifest to image destination
80f4c7a5e91618546dce5b4fe60cf03b14c0f9efa7e40157278d122772ced8d2

real	0m2.842s
user	0m2.854s
sys	0m0.568s
[docker.io/library/postgres@sha256:485935f94cc7165afa896978809c37b592dc07f0a37d2c8f645f12412d0212c8 docker.io/library/postgres@sha256:f1c3376c26f2609ab9f29f71f824103fe2fcd8ee0346485cb6122a4f93df6f94]
$ sudo tail -n +62 /var/log/squid/access.log    # the lines this pull added
1789120848.603     31 10.99.14.2 TCP_TUNNEL/200 5430 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120849.395    114 10.99.14.2 TCP_TUNNEL/200 1276778 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120849.395    114 10.99.14.2 TCP_TUNNEL/200 6483138 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120849.584    303 10.99.14.2 TCP_TUNNEL/200 8253509 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120850.426   1823 10.99.14.2 TCP_TUNNEL/200 115166742 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120851.349   2068 10.99.14.2 TCP_TUNNEL/200 21313 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120851.349   2068 10.99.14.2 TCP_TUNNEL/200 7685 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
```

Two things the work order's pass line did not anticipate.

**The digest names `docker.io`, and that is podman, not a miss.** A
`[[registry]] location` rewrite is transparent by design: the image is stored
under the name that was asked for, and `RepoDigests` carries it. Where the
bytes came from is in podman's own resolution and in squid's log — every
`CONNECT` of the pull is to `harbor.tailcbcc0e.ts.net:443`, and the whole
session's log has no line naming an upstream registry:

```
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=HTTPS_PROXY=$P -- sh -c 'podman pull --log-level=debug docker.io/library/postgres:16 2>&1 | grep -iE "harbor|registry-1" | head -4'
time="2026-09-11T03:01:16-07:00" level=debug msg="Loading registries configuration \"/etc/containers/registries.conf.d/harbor.conf\""
time="2026-09-11T03:01:16-07:00" level=debug msg="Trying to access \"harbor.tailcbcc0e.ts.net/dockerhub-cache/library/postgres:16\""
time="2026-09-11T03:01:16-07:00" level=debug msg="No credentials matching harbor.tailcbcc0e.ts.net/dockerhub-cache/library/postgres found in /run/user/1000/containers/auth.json"
time="2026-09-11T03:01:16-07:00" level=debug msg="No credentials matching harbor.tailcbcc0e.ts.net/dockerhub-cache/library/postgres found in /home/agent/.config/containers/auth.json"
$ sudo grep -cE 'docker.io|registry-1' /var/log/squid/access.log    # end of session
0
```

A check that wants "came through Harbor" reads squid's log or podman's debug
line, not `RepoDigests`; the design's sentence "the digest names Harbor" in
the work order should not become an assertion in a spec.

**This machine-cold pull was Harbor-warm.** Harbor's API gives the artifact's
`push_time` as 02:47 PDT — the first attempt at this node pulled it, minutes
before this session started — so the 115 MB layer in 1.8 s above is the LAN
figure, not the Docker Hub one. For a Harbor-cold number, a tag Harbor did not
hold:

```
$ curl -s -o /dev/null -w "%{http_code}\n" "https://harbor.tailcbcc0e.ts.net/api/v2.0/projects/dockerhub-cache/repositories/library%252Fpostgres/artifacts/16.3"
404
$ sudo systemd-run -M mill-e1 --pipe --wait --quiet --collect -p User=agent -p Environment=HTTPS_PROXY=$P -- sh -c 'time podman pull docker.io/library/postgres:16.3' 2>&1 | grep -v 'Copying blob'
Trying to pull docker.io/library/postgres:16.3...
Getting image source signatures
Copying config sha256:07a4ee949b9e7851d9ef860fd36f486aa821f23e1c17939244155fe825eaf79a
Writing manifest to image destination
07a4ee949b9e7851d9ef860fd36f486aa821f23e1c17939244155fe825eaf79a

real	0m5.807s
user	0m3.623s
sys	0m0.807s
$ squid access.log, this pull
1789120920.759     16 10.99.14.2 TCP_TUNNEL/200 5431 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120923.841   3081 10.99.14.2 TCP_TUNNEL/200 1256123 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120923.954   1611 10.99.14.2 TCP_TUNNEL/200 1469124 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120923.960   1617 10.99.14.2 TCP_TUNNEL/200 4593929 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120925.602   3259 10.99.14.2 TCP_TUNNEL/200 110296823 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120926.492   4150 10.99.14.2 TCP_TUNNEL/200 29474652 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120926.492   4150 10.99.14.2 TCP_TUNNEL/200 8141008 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
```

5.8 s with Harbor fetching from Docker Hub on the machine's behalf; still only
Harbor in the log.

### Second pull, from a fresh machine

`mill-e1` stopped, `mill-e11` booted from the image with an empty store, on
the same veth address (squid kept its bound socket across the link's
recreation and answered without a restart):

```
$ sudo systemctl stop mill-run-e1; sudo machinectl list --no-legend
qemu-1-omarchy vm libvirt-qemu - - -
$ /tmp/spike-out/boot.sh e11 14
Running as unit: mill-run-e11.service; invocation ID: 321f04ffcb5d4f9a8c8d4d3d707337f0
running
                       State: routable (configured)
                     Address: 10.99.14.2
$ systemctl is-active squid; sudo ss -ltn | grep 3128
active
LISTEN 0      4096                    10.99.14.1:3128       0.0.0.0:*
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- podman images
REPOSITORY  TAG         IMAGE ID    CREATED     SIZE
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -p Environment=HTTPS_PROXY=$P -- sh -c 'time podman pull docker.io/library/postgres:16; podman image inspect --format "{{.RepoDigests}}" docker.io/library/postgres:16'
Trying to pull docker.io/library/postgres:16...
Getting image source signatures
Copying config sha256:80f4c7a5e91618546dce5b4fe60cf03b14c0f9efa7e40157278d122772ced8d2
Writing manifest to image destination
80f4c7a5e91618546dce5b4fe60cf03b14c0f9efa7e40157278d122772ced8d2

real	0m2.444s
user	0m3.538s
sys	0m0.666s
[docker.io/library/postgres@sha256:485935f94cc7165afa896978809c37b592dc07f0a37d2c8f645f12412d0212c8 docker.io/library/postgres@sha256:f1c3376c26f2609ab9f29f71f824103fe2fcd8ee0346485cb6122a4f93df6f94]
$ squid access.log, this pull
1789120953.358     24 10.99.14.2 TCP_TUNNEL/200 5430 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120953.780    100 10.99.14.2 TCP_TUNNEL/200 1284652 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120953.809    129 10.99.14.2 TCP_TUNNEL/200 6484953 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120953.982    302 10.99.14.2 TCP_TUNNEL/200 8253575 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120954.033    352 10.99.14.2 TCP_TUNNEL/200 29957859 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120954.815   1457 10.99.14.2 TCP_TUNNEL/200 115167223 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120955.744   1954 10.99.14.2 TCP_TUNNEL/200 4911 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
1789120955.745   2064 10.99.14.2 TCP_TUNNEL/200 13651 CONNECT harbor.tailcbcc0e.ts.net:443 - HIER_DIRECT/100.114.13.91 -
```

| | `podman pull` |
|---|---|
| Harbor-cold, machine-cold (`postgres:16.3`) | 5.8 s |
| Harbor-warm, machine-cold (`postgres:16`, mill-e1) | 2.8 s |
| Harbor-warm, fresh machine (`postgres:16`, mill-e11) | 2.4 s |

**Verdict: E11 passes. Every pull resolved to `harbor.tailcbcc0e.ts.net/dockerhub-cache/library/postgres` (podman's debug line) and squid logged `CONNECT`s to Harbor only, none to `docker.io` or `registry-1.docker.io` across the whole session; the pull from a fresh machine is 2.4 s for a 464 MB image, a LAN fetch. The `registries.conf.d` file in the image is right as written. `RepoDigests` keeps the `docker.io` name under a `location` rewrite, so the proof of provenance is the proxy log, not the digest.**

---

## E12 — The dnf shim

The shim is `spikes/nspawn/dnf`, written into the running `mill-e11`'s
`/usr/local/bin/dnf` rather than a rebuilt image (the image is untouched; the
Containerfile does not carry it yet — that is `mill#59`'s). The sudoers line
was already in the image from the boot spike. `agent`'s `PATH` puts
`/usr/local/bin` first:

```
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -- sh -c 'cat > /usr/local/bin/dnf && chmod 0755 /usr/local/bin/dnf && ls -l /usr/local/bin/dnf && cat /etc/sudoers.d/agent-dnf' < spikes/nspawn/dnf
-rwxr-xr-x. 1 root root 1006 Sep 11 03:03 /usr/local/bin/dnf
agent ALL=(root) NOPASSWD: /usr/bin/dnf install *
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- sh -lc 'echo $PATH; command -v dnf'
/usr/local/bin:/usr/bin
/usr/local/bin/dnf
```

### First attempt: the proxy option in the wrong place

`sudo` resets the environment, so `HTTPS_PROXY` on the exec never reaches
`/usr/bin/dnf`; the shim has to pass it as `--setopt=proxy=`. The first
version put that option before the subcommand, and the sudoers rule is
`/usr/bin/dnf install *` — the `install` must be the first argument:

```
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -p Environment=HTTPS_PROXY=$P -- dnf install -y vips-devel 2>&1 | tail -3
sudo: a terminal is required to read the password; either use the -S option to read from standard input or configure an askpass helper
sudo: a password is required
rc=1
```

`sudo /usr/bin/dnf install --setopt=proxy=… <packages>` matches the rule.
That is the shim as committed.

### The install as `agent`, and the removal

```
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -p Environment=HTTPS_PROXY=$P -- dnf install -y vips-devel 2>&1 | tail -3
mill: installed vips-devel into an ephemeral machine; the next run's image will not have it.
mill: declare it in mill.toml so the image does. Under [image], add:
mill:   packages = ["vips-devel"]
$ head -5 /tmp/spike-out/e12-install.log; sed -n '/Transaction Summary/,/^$/p' /tmp/spike-out/e12-install.log; tail -6 /tmp/spike-out/e12-install.log    # the whole output, 841 lines, went to a file
Updating and loading repositories:
 Fedora 44 openh264 (From Cisco) - x86_ 100% |   5.4 KiB/s |   5.3 KiB |  00m01s
 Fedora 44 - x86_64 - Updates           100% |  11.5 MiB/s |  11.7 MiB |  00m01s
 Fedora 44 - x86_64                     100% |  27.7 MiB/s |  38.3 MiB |  00m01s
Repositories loaded.
Transaction Summary:
 Installing:       273 packages

[274/275] Installing open-sans-fonts-0: 100% | 417.4 MiB/s |   2.1 MiB |  00m00s
[275/275] Installing python3-pip-0:26.0 100% |  15.7 MiB/s |  11.7 MiB |  00m01s
Complete!
mill: installed vips-devel into an ephemeral machine; the next run's image will not have it.
mill: declare it in mill.toml so the image does. Under [image], add:
mill:   packages = ["vips-devel"]
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- sh -c 'rpm -q vips-devel; pkg-config --modversion vips'
vips-devel-8.18.3-2.fc44.x86_64
8.18.3
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- sudo /usr/bin/dnf remove -y vips-devel; echo "rc=$?"
sudo: a terminal is required to read the password; either use the -S option to read from standard input or configure an askpass helper
sudo: a password is required
rc=1
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- sudo -n /usr/bin/dnf remove -y vips-devel; echo rc=$?
sudo: a password is required
rc=1
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- dnf remove -y vips-devel; echo rc=$?   # through the shim
The requested operation requires superuser privileges. Please log in as a user with elevated rights, or use the "--assumeno" or "--downloadonly" options to run the command without modifying the system state.
rc=1
$ sudo systemd-run -M mill-e11 --pipe --wait --quiet --collect -p User=agent -- sudo -l
User agent may run the following commands on mill-e11:
    (root) NOPASSWD: /usr/bin/dnf install *
```

273 packages (vips pulls ImageMagick, fonts, a pip) in 16.9 s wall through
the proxy, the mirrors at 27 MiB/s. The last three lines of the output are
the hint; `sudo … remove` is refused by sudoers for want of a password
`agent` does not have, and `remove` through the shim is dnf's own refusal as
an unprivileged user.

**The hint text**, three lines, package list in TOML form so it can be pasted:

```
mill: installed vips-devel into an ephemeral machine; the next run's image will not have it.
mill: declare it in mill.toml so the image does. Under [image], add:
mill:   packages = ["vips-devel"]
```

### The report to a host socket

`docs/sandbox.md` §The dnf shim has the shim report the install over the
workspace socket. A listener on the host and `curl --unix-socket` from inside
is enough to show the path exists; the shim posts to
`/run/mill/report.sock` if it is there. First, into the running machine:

```
$ sudo machinectl bind --mkdir mill-e11 /tmp/spike-out/report.sock /run/mill/report.sock; echo rc=$?
Failed to bind mount: Can't bind mount on container with user namespacing applied.
rc=1
```

`machinectl bind` refuses under `--private-users`: **every bind is decided at
boot**, the socket directory included, which §Provision already implies but
§Filesystem should say. So a third machine, with the directory holding the
socket bound at boot with `:idmap`, and a 12-line `http.server` on the host
end:

```
$ (setsid nohup python3 /tmp/spike-out/report-sock.py /tmp/spike-out/millrun/report.sock > /tmp/spike-out/report.log 2>&1 &); ls -l /tmp/spike-out/millrun/
srw-rw-rw-. 1 ivy ivy 0 Sep 11 03:04 report.sock
$ /tmp/spike-out/boot.sh e12 15 --bind=/tmp/spike-out/millrun:/run/mill:idmap
Running as unit: mill-run-e12.service; invocation ID: 13593cacb8654daea74d9f14c3278b06
running
$ sudo systemd-run -M mill-e12 --pipe --wait --quiet --collect -p User=agent -- sh -c 'ls -l /run/mill/report.sock; curl -sS --unix-socket /run/mill/report.sock -o /dev/null -w "%{http_code}\n" -X POST --data "installed \"vips-devel\"" http://mill/dnf'
srw-rw-rw-. 1 agent agent 0 Sep 11 03:04 /run/mill/report.sock
204
$ cat /tmp/spike-out/report.log   # on the host
report /dnf 'installed "vips-devel"'
```

The socket is `agent`'s inside (the `:idmap` again), the POST lands on the
host with its body, `204` back. The report is feasible with nothing beyond
`curl` in the image; the workspace socket's protocol is `mill#64`'s.

**Verdict: E12 passes. `dnf install -y vips-devel` succeeds as `agent` through the shim and the sudoers rule, its last three lines are the hint above, and `sudo /usr/bin/dnf remove` is refused (`a password is required`, `rc=1`). The shim must put `--setopt=proxy=` after `install` or the sudoers pattern does not match. A report to a host socket works over a directory bound at boot — `machinectl bind` cannot add one to a running user-namespaced machine.**

### The executable form

The four scenarios are `spikes/nspawn/egress_test.go`, `Label("live")`, in the
same suite as E0–E4 and E14: each boots its machine, addresses the veth,
loads `egress.nft`, copies `squid.conf` into place and starts squid, and takes
all of it down with the spec. Run after the sections above, with the host
torn down between:

```
$ MILL_SANDBOX_HOST=1 ginkgo --label-filter=live --focus-file egress_test.go --timeout=30m ./spikes/nspawn
Will run 4 of 11 specs
••
• [FAILED] [40.819 seconds]
Egress, the db suite, Harbor and the shim on a real machine [It] Pulls resolve through Harbor [live, Serial]
  [FAILED] Expected
      <string>: 
  to contain substring
      <string>: harbor.tailcbcc0e.ts.net/dockerhub-cache/library/postgres:16
•SSSSSSS
Ran 4 of 11 Specs in 512.433 seconds
FAIL! -- 3 Passed | 1 Failed | 0 Pending | 7 Skipped
$ MILL_SANDBOX_HOST=1 ginkgo --label-filter=live --focus 'Pulls resolve through Harbor' --timeout=10m ./spikes/nspawn    # the grep for podman's debug line fixed
Will run 1 of 11 specs
SSSSSSSSS•S
Ran 1 of 11 Specs in 40.967 seconds
SUCCESS! -- 1 Passed | 0 Failed | 0 Pending | 10 Skipped
```

The first run's failure was the spec's own `grep` against podman's debug
output (the quotes in `Trying to access \"…\"` are escaped in the log line),
not the machine's; fixed, the scenario passes. The 512 s is mostly the db
scenario waiting out ryuk's five-minute window to assert that no container
remains. `make test` runs the suite and every live spec skips.

---

## Teardown (E5–E12)

Only what this node created: the `mill-e*` machines and their `mill-run-e*`
units, the table, and squid's process (the package stays for
`homestead#34`). The firewalld admission was runtime-only and is removed
too. The image subvolume stays.

```
$ sudo systemctl stop mill-run-e1 2>/dev/null; echo rc=$?
rc=5
$ sudo systemctl stop mill-run-e11 2>/dev/null; echo rc=$?
rc=0
$ sudo systemctl stop mill-run-e12 2>/dev/null; echo rc=$?
rc=0
$ sudo nft delete table inet mill; echo rc=$?
rc=0
$ sudo systemctl stop squid; systemctl is-active squid
inactive
$ sudo firewall-cmd --zone=trusted --remove-interface='ve-mill+'
success
$ sudo machinectl list --no-pager; sudo nft list table inet mill; systemctl list-units "mill-run-e*" --all --no-legend; sudo btrfs subvolume list /var/lib/machines; ls -l /etc/systemd/system/mill-machine@.service; rpm -q squid
MACHINE        CLASS     SERVICE        OS     VERSION ADDRESSES
mill-sel       container systemd-nspawn fedora 44      -
qemu-1-omarchy vm        libvirt-qemu   -      -       -

2 machines listed.
Error: No such file or directory
list table inet mill
                ^^^^
ID 256 gen 311 top level 5 path @mill
ID 257 gen 322 top level 5 path @machines
ID 298 gen 159 top level 257 path mill-image-spike
ID 328 gen 322 top level 257 path .#machine.mill-image-spikedb1903a60fb916d5
-rw-r--r--. 1 root root 1640 Sep 11 01:03 /etc/systemd/system/mill-machine@.service
squid-7.6-1.fc44.x86_64
$ sudo ausearch -m avc -ts 02:50:00
<no matches>
```

`rc=5` on `mill-run-e1` is "no such unit": it was stopped in E11. `mill-sel`
and the snapshot `.#machine.mill-image-spikedb1903a60fb916d5` are another
node's machine on this host (the SELinux spike), running from the same image;
not this node's, left alone. No `mill-e*` machine, no `inet mill` table, no
AVC during the session.

### What the design takes from E5–E12

- **§Network.** The static table is the work order's four-rule pair plus a
  `nat postrouting` masquerade for the forwarded DNS; `spikes/nspawn/egress.nft`
  is that. The `ve-mill*` links must also be admitted to firewalld's
  `trusted` zone (runtime `--add-interface='ve-mill+'`, or a permanent zone
  binding in homestead), since `core`'s default zone is `drop` and an accept
  in `inet mill` does not skip `inet firewalld`. Squid must bind an address
  that exists before any machine does — a dummy link, or the wildcard with the
  `machines` ACL — or be ordered after the first veth. Squid needs a rule
  denying RFC 1918 destinations, because the table cannot see what the proxy
  connects to. The proxy variables go into the environment in both cases
  (`curl` reads only `http_proxy` for plain HTTP), and into `agent`'s user
  manager (`environment.d` or a `podman.service` drop-in), because podman's
  API service pulls with its own environment, not the exec's. DNS goes
  straight to `1.1.1.1` through the forward here; a resolver on the host side
  of the veth (or `systemd-resolved`'s stub on the machine's `host0`) would
  let the forward chain drop DNS too.
- **§Testing.** The db suite runs inside as it does on the host: `agent`'s
  `/run/user/1000/podman/podman.sock`, ryuk on, 20 s warm. The command is
  `mise exec -- make test-db` from an exec; a session's shell has
  `mise activate`. A Go module cache under §Caching halves the cold run.
- **§Filesystem.** Every bind is fixed at boot — `machinectl bind` refuses a
  user-namespaced machine — so the socket directory, the skill and the caches
  are all in the provision's nspawn line. The operator's checkout was writable
  as `agent` with `:idmap` because the operator is uid 1000; a workspace mill
  creates is uid 1000 by construction.
- **§Images.** `registries.conf.d/harbor.conf` as the Containerfile writes it
  is right; `RepoDigests` will name `docker.io`, and provenance is squid's
  log. `mill#59` adds `spikes/nspawn/dnf` to the image at `/usr/local/bin/dnf`.
- **§The dnf shim / `mill#64`.** The shim passes the proxy as
  `--setopt=proxy=` after `install`; the hint is the three `mill:` lines
  above; the report is a `curl --unix-socket` POST to a socket in a directory
  bound at boot.

---

## E9 — Cache lineage: restore, promote, and what it costs

Run 2026-09-11 06:27 – 06:32 PDT on the same host. The image subvolume from
the boot spike was still present (`ID 298 … mill-image-spike`; the work order
allowed for its absence), so no rebuild. Homestead's
`/etc/systemd/system/mill-machine@.service` (1640 bytes, `Sep 11 01:03`),
its `mill-nftables.service`, squid, the `mill-host` link and the
`mill-image-e7`/`mill-image-e8` subvolumes of other nodes were left alone.
Machines were booted directly with `systemd-run --unit=mill-run-e*`, E1's
line plus `:idmap` on every bind.

Every `btrfs` command below runs as the operator (uid 1000) with no `sudo`
unless the prompt shows one; `/var/lib/mill` is the operator's and is mounted
`user_subvol_rm_allowed` (§What was under test).

### The fill, on the host

Squid on this host is homestead's now, and its `squid.conf` (Fedora's
default, `-rw-r----- root squid 1171 Sep 11 04:03`, an empty
`/etc/squid/conf.d/`) has no `machines` ACL: it admits `localhost` only, so
a machine cannot fetch through it. The probe, from a non-loopback source on
the host:

```
$ curl -sS -o /dev/null -w "%{http_code}\n" -x http://10.99.255.1:3128 --interface 10.99.255.1 https://proxy.golang.org/; sudo tail -1 /var/log/squid/access.log
curl: (56) CONNECT tunnel failed, response 403
000
1789133238.531      0 10.99.255.1 TCP_DENIED/403 3367 CONNECT proxy.golang.org:443 - HIER_NONE/- text/html
```

That is a finding against the network landing, not this node's; per the work
order the fill ran on the host with the same `GOCACHE`, and the second build
inside the machine used the host's Go toolchain bound read-only at `/opt/go`
with `GOPROXY=off` (the module cache is in the lineage, so nothing is
fetched). The subvolume, created as the operator:

```
$ date +%T; id -u; btrfs subvolume create /var/lib/mill/cache-e9-target; echo rc=$?; ls -ld /var/lib/mill/cache-e9-target
06:27:18
1000
Create subvolume '/var/lib/mill/cache-e9-target'
rc=0
drwxr-xr-x 1 ivy ivy 0 Sep 11 06:27 /var/lib/mill/cache-e9-target
```

The work order's fill — one `go build ./...` and `go test -run xxx ./...` —
is 704M for this repository, so the cache was grown to "a few gigabytes"
with builds that produce distinct cache entries: `-race`, and `GOARCH=arm64`,
`riscv64`, `386` (a `darwin` build was tried too; it fails on a
linux-specific type, which is fine for filling a cache). The `go:
downloading` lines are elided.

```
$ cd /home/ivy/src/github.com/ivy/mill; time (GOCACHE=/var/lib/mill/cache-e9-target GOMODCACHE=/var/lib/mill/cache-e9-target/mod GOTOOLCHAIN=local go build ./... && GOCACHE=/var/lib/mill/cache-e9-target GOMODCACHE=/var/lib/mill/cache-e9-target/mod GOTOOLCHAIN=local go test -count=1 -run xxx ./... 2>&1 | tail -5)
go: downloading google.golang.org/protobuf v1.36.12
[… 66 more]
ok  	github.com/ivy/mill/internal/workspace/socket	0.004s [no tests to run]
ok  	github.com/ivy/mill/proto/contract	0.002s [no tests to run]
ok  	github.com/ivy/mill/sidecar	0.002s [no tests to run]
ok  	github.com/ivy/mill/spikes/nspawn	0.002s [no tests to run]
ok  	github.com/ivy/mill/tests/acceptance	0.023s [no tests to run]

real	0m11.603s
user	1m33.510s
sys	0m12.515s
$ du -sh /var/lib/mill/cache-e9-target; du -s /var/lib/mill/cache-e9-target; find /var/lib/mill/cache-e9-target -type f | wc -l
704M	/var/lib/mill/cache-e9-target
720876	/var/lib/mill/cache-e9-target
15633
$ cd /home/ivy/src/github.com/ivy/mill; export GOCACHE=/var/lib/mill/cache-e9-target GOMODCACHE=/var/lib/mill/cache-e9-target/mod GOTOOLCHAIN=local; time (go build -race ./... && go test -race -count=1 -run xxx ./... >/dev/null && GOARCH=arm64 go build ./... && GOARCH=arm64 go test -count=1 -run xxx -exec /bin/true ./... >/dev/null; GOARCH=arm64 go vet ./... >/dev/null 2>&1; go vet ./... >/dev/null 2>&1; GOOS=darwin GOARCH=arm64 go build ./... ; GOOS=darwin GOARCH=arm64 go test -count=1 -run xxx -exec /bin/true ./... > /dev/null); du -sh /var/lib/mill/cache-e9-target
go: downloading github.com/ebitengine/purego v0.10.1
# github.com/ivy/mill/internal/workspace/machine
internal/workspace/machine/systemd.go:208:25: cannot use stat.Type (variable of type uint32) as int64 value in argument to filesystemName
# github.com/ivy/mill/internal/workspace/machine
internal/workspace/machine/systemd.go:208:25: cannot use stat.Type (variable of type uint32) as int64 value in argument to filesystemName

real	0m37.915s
user	4m57.832s
sys	0m39.706s
2.0G	/var/lib/mill/cache-e9-target
$ cd /home/ivy/src/github.com/ivy/mill; export GOCACHE=/var/lib/mill/cache-e9-target GOMODCACHE=/var/lib/mill/cache-e9-target/mod GOTOOLCHAIN=local; time (GOARCH=riscv64 go build ./... && GOARCH=riscv64 go test -count=1 -run xxx -exec /bin/true ./... >/dev/null; GOARCH=386 go build ./... && GOARCH=386 go test -count=1 -run xxx -exec /bin/true ./... >/dev/null); du -sh /var/lib/mill/cache-e9-target; du -s /var/lib/mill/cache-e9-target; find /var/lib/mill/cache-e9-target -type f | wc -l; df -h /var/lib/mill
# github.com/ivy/mill/internal/workspace/machine
internal/workspace/machine/systemd.go:208:25: cannot use stat.Type (variable of type int32) as int64 value in argument to filesystemName

real	0m16.449s
user	2m31.636s
sys	0m18.625s
2.8G	/var/lib/mill/cache-e9-target
2834100	/var/lib/mill/cache-e9-target
31604
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.4G  929G   1% /var/lib/mill
```

2.8G apparent (`du`), 31 604 files; `df` moved 767M → 1.4G because the
filesystem is `compress=zstd:1` and a Go build cache compresses about 2:1.
Both numbers matter to `mill#67`: `du -s` is the estimate §Promote records,
and it overstates what eviction gets back by about half on this filesystem.

### Promote gen-1, restore the run's copy

```
$ btrfs subvolume snapshot -r /var/lib/mill/cache-e9-target /var/lib/mill/cache-e9-gen-1; echo rc=$?
Create readonly snapshot of '/var/lib/mill/cache-e9-target' in '/var/lib/mill/cache-e9-gen-1'
rc=0
$ time btrfs subvolume snapshot /var/lib/mill/cache-e9-gen-1 /var/lib/mill/run-e9-target; echo rc=$?
Create snapshot of '/var/lib/mill/cache-e9-gen-1' in '/var/lib/mill/run-e9-target'

real	0m0.001s
user	0m0.001s
sys	0m0.000s
rc=0
$ du -s /var/lib/mill/cache-e9-gen-1; du -sh /var/lib/mill/cache-e9-gen-1; btrfs subvolume show /var/lib/mill/cache-e9-gen-1 | grep -E "Flags|Parent UUID|Received"; btrfs subvolume show /var/lib/mill/run-e9-target | grep -E "Flags|Parent UUID"; ls -ld /var/lib/mill/run-e9-target; df -h /var/lib/mill
2834100	/var/lib/mill/cache-e9-gen-1
2.8G	/var/lib/mill/cache-e9-gen-1
ERROR: Could not search B-tree: Operation not permitted
ERROR: Could not search B-tree: Operation not permitted
drwxr-xr-x 1 ivy ivy 1058 Sep 11 06:27 /var/lib/mill/run-e9-target
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.6G  929G   1% /var/lib/mill
```

**Restore: 1 ms** for 2.8G. The generation's size estimate is `du -s` =
`2834100` KiB. `btrfs subvolume show` is the first thing that wanted root:
it walks the tree with a search ioctl that needs `CAP_SYS_ADMIN`. It is
informational and nothing in §Caching needs it — `btrfs subvolume list`
is the same (below) — but `mill cache ls` cannot use either; it lists
directories and reads its own `manifest.json`.

### The run, on the restored copy

Booted with the checkout at `/work`, the run's snapshot at `/work/.cache/go`
and the host's Go at `/opt/go`, every bind `:idmap`. The mount point
`/work/.cache/go` was created in the checkout beforehand (nspawn would
otherwise create it, as root, inside the operator's tree).

```
$ date +%T.%N; sudo systemd-run --unit=mill-run-e9 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e9 --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false --bind=/home/ivy/src/github.com/ivy/mill:/work:idmap --bind=/var/lib/mill/run-e9-target:/work/.cache/go:idmap --bind-ro=/home/ivy/.local/share/mise/installs/go/1.25.7:/opt/go:idmap; until sudo systemd-run -M mill-e9 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.2; done; date +%T.%N; sudo systemd-run -M mill-e9 --pipe --wait --quiet --collect -- systemctl is-system-running --wait
06:29:20.936734212
Running as unit: mill-run-e9.service; invocation ID: cba5ce369b914174a511dce42f5397fb
06:29:21.211297210
running
$ sudo systemd-run -M mill-e9 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -- sh -c "id; ls -ld /work /work/.cache/go /opt/go; findmnt -no SOURCE,FSTYPE,OPTIONS /work/.cache/go; touch /work/.cache/go/.e9-probe && echo writable && rm /work/.cache/go/.e9-probe; /opt/go/bin/go version"
uid=1000(agent) gid=1000(agent) groups=1000(agent)
drwxr-xr-x. 10 agent agent 4096 Feb  7  2026 /opt/go
drwxr-xr-x. 18 agent agent 4096 Sep 11 06:29 /work
drwxr-xr-x   1 agent agent 1058 Sep 11 06:27 /work/.cache/go
/dev/mapper/mill[/@mill/run-e9-target] btrfs rw,noatime,idmapped,seclabel,compress=zstd:1,ssd,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=399,subvol=/@mill/run-e9-target
writable
go version go1.25.7 linux/amd64
$ time sudo systemd-run -M mill-e9 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -p Environment=GOCACHE=/work/.cache/go -p Environment=GOMODCACHE=/work/.cache/go/mod -p Environment=GOTOOLCHAIN=local -p Environment=GOPROXY=off -p Environment=GOFLAGS=-mod=mod -p Environment=HOME=/home/agent -- sh -c "/opt/go/bin/go build ./... && /opt/go/bin/go test -count=1 -run xxx ./... | tail -3 && /opt/go/bin/go build -tags e9 -o /tmp/mill-e9 ./cmd/mill && ls -l /tmp/mill-e9"; echo rc=$?
ok  	github.com/ivy/mill/sidecar	0.002s [no tests to run]
ok  	github.com/ivy/mill/spikes/nspawn	0.002s [no tests to run]
ok  	github.com/ivy/mill/tests/acceptance	0.005s [no tests to run]
-rwxr-xr-x. 1 agent agent 34520315 Sep 11 06:29 /tmp/mill-e9

real	0m7.856s
user	0m0.007s
sys	0m0.007s
rc=0
```

A subvolume binds like any directory: `--bind=…:idmap` and nothing else —
it shows up inside as its own btrfs mount (`subvol=/@mill/run-e9-target`,
`idmapped`), owned by `agent`, writable. The whole build plus every test
binary, warm from the lineage and with no network, is 7.9 s (11.6 s cold on
the host with a hot module cache, 43.9 s cold inside in E6). The
`-tags e9` link is the guaranteed new write: Go's build cache is
content-addressed and a warm rebuild writes only what it has not seen.

### Promote gen-2, and the file that proves the generations apart

```
$ find /var/lib/mill/run-e9-target -type f -newer /var/lib/mill/cache-e9-gen-1 | wc -l; find /var/lib/mill/run-e9-target -type f -newer /var/lib/mill/cache-e9-gen-1 -size +1M | head -3; du -sh /var/lib/mill/run-e9-target; df -h /var/lib/mill
19567
/var/lib/mill/run-e9-target/00/009539ba8564ed2c6bb0378076fbcbebe4772d49b9a22c69fdce210cedd5aa4a-d
/var/lib/mill/run-e9-target/01/01071e4df980d7435f53b4c2b89cd53dc093fbd41635d65481ba52f029c74048-d
/var/lib/mill/run-e9-target/01/0145c7c8ec91fcca3ecbc864ec0bbcf1e2c049c08a1ce16cfc0dad953fd11e0a-d
3.0G	/var/lib/mill/run-e9-target
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.6G  929G   1% /var/lib/mill
$ time btrfs subvolume snapshot -r /var/lib/mill/run-e9-target /var/lib/mill/cache-e9-gen-2; echo rc=$?
Create readonly snapshot of '/var/lib/mill/run-e9-target' in '/var/lib/mill/cache-e9-gen-2'

real	0m0.052s
user	0m0.000s
sys	0m0.013s
rc=0
```

**Promote: 52 ms** for 3.0G (the run's copy grew 2.8G → 3.0G). The 19 567
"newer" files are mostly mtime touches — Go bumps the mtime of every entry it
uses, for its own trimming — so mtime does not tell the two generations
apart; the set difference of their file lists does:

```
$ cd /var/lib/mill; comm -13 <(cd cache-e9-gen-1 && find . -type f | sort) <(cd cache-e9-gen-2 && find . -type f | sort) > /tmp/spike-out/e9/new-in-gen-2; wc -l < /tmp/spike-out/e9/new-in-gen-2; f=$(grep -- "-d$" /tmp/spike-out/e9/new-in-gen-2 | xargs -I{} sh -c "printf \"%s %s\n\" \$(stat -c %s cache-e9-gen-2/{}) {}" | sort -n | tail -1 | cut -d" " -f2); echo "file: $f"; ls -l cache-e9-gen-2/$f; ls -l cache-e9-gen-1/$f; ls -l run-e9-target/$f; sha256sum cache-e9-gen-2/$f run-e9-target/$f
2563
file: ./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d
-rw-r--r--. 1 ivy ivy 22348106 Sep 11 06:29 cache-e9-gen-2/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d
ls: cannot access 'cache-e9-gen-1/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d': No such file or directory
-rw-r--r--. 1 ivy ivy 22348106 Sep 11 06:29 run-e9-target/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d
1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6  cache-e9-gen-2/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d
1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6  run-e9-target/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d
```

2 563 files are in gen-2 and not in gen-1. The largest,
`1f/1fdbddf5…-d` (22 MB, the `-tags e9` link output, written at 06:29 by
the machine), is in gen-2 and the run's copy with the same SHA and absent
from gen-1.

### Delete gen-1 under the live run

Three things came out of this step that the work order did not expect, all
about what an unprivileged owner may do with `user_subvol_rm_allowed`:

```
$ df -h /var/lib/mill; btrfs subvolume delete /var/lib/mill/cache-e9-gen-1; echo rc=$?; df -h /var/lib/mill; time btrfs subvolume sync /var/lib/mill; df -h /var/lib/mill
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.7G  928G   1% /var/lib/mill
ERROR: Could not destroy subvolume/snapshot: Read-only file system
Delete subvolume 398 (no-commit): '/var/lib/mill/cache-e9-gen-1'
rc=1
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.7G  928G   1% /var/lib/mill
ERROR: Could not search B-tree: Operation not permitted

real	0m0.001s
user	0m0.000s
sys	0m0.001s
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.7G  928G   1% /var/lib/mill
```

1. **A read-only snapshot cannot be deleted by its unprivileged owner**:
   `EROFS`. `user_subvol_rm_allowed` lets the owner delete a subvolume it
   may write; a `-r` snapshot is not writable, so the check fails before the
   ownership matters. The owner may flip the flag first, and then the delete
   goes through:

```
$ btrfs property get -ts /var/lib/mill/cache-e9-gen-1 ro; btrfs property set -ts /var/lib/mill/cache-e9-gen-1 ro false; echo rc=$?; btrfs property get -ts /var/lib/mill/cache-e9-gen-1 ro; btrfs subvolume delete /var/lib/mill/cache-e9-gen-1; echo rc=$?; df -h /var/lib/mill; time btrfs subvolume sync /var/lib/mill; echo rc=$?; df -h /var/lib/mill
ro=true
rc=0
ro=false
Delete subvolume 398 (no-commit): '/var/lib/mill/cache-e9-gen-1'
rc=0
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.7G  928G   1% /var/lib/mill
ERROR: Could not search B-tree: Operation not permitted
rc=1
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.7G  928G   1% /var/lib/mill
```

2. **`btrfs subvolume sync` needs root** — it polls the deleted-subvolume
   list with the same tree search `show` and `list` use. Without it, and
   without `sudo`, the deletion still completes: the kernel's cleaner had
   dropped gen-1 before the `sudo` sync below ran, which found nothing to
   wait for.

3. The run was undisturbed. The file above still reads with the same SHA
   from inside, the mount is still `subvolid=399`, and a rebuild reproduces
   the same binary from the cache in a second:

```
$ sudo systemd-run -M mill-e9 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -- sh -c 'sha256sum /work/.cache/go/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d; findmnt -no OPTIONS /work/.cache/go | tr , "\n" | grep subvol'
1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6  /work/.cache/go/./1f/1fdbddf5258525e97a22e79417038cf9c75f39885417b4d8b2001b4754a620f6-d
user_subvol_rm_allowed
subvolid=399
subvol=/@mill/run-e9-target
$ time sudo systemd-run -M mill-e9 --pipe --wait --quiet --collect -p User=agent -p WorkingDirectory=/work -p Environment=GOCACHE=/work/.cache/go -p Environment=GOMODCACHE=/work/.cache/go/mod -p Environment=GOTOOLCHAIN=local -p Environment=GOPROXY=off -p Environment=GOFLAGS=-mod=mod -p Environment=HOME=/home/agent -- sh -c "/opt/go/bin/go build ./... && /opt/go/bin/go build -tags e9 -o /tmp/mill-e9b ./cmd/mill && cmp /tmp/mill-e9 /tmp/mill-e9b && echo same-binary"; echo rc=$?
same-binary

real	0m1.002s
user	0m0.007s
sys	0m0.005s
rc=0
$ btrfs subvolume list /var/lib/mill 2>&1; sudo btrfs subvolume list /var/lib/mill | grep e9
ERROR: can't perform the search: Operation not permitted
ID 397 gen 581 top level 256 path cache-e9-target
ID 398 gen 581 top level 256 path cache-e9-gen-1
ID 399 gen 584 top level 256 path run-e9-target
ID 401 gen 584 top level 256 path cache-e9-gen-2
$ time sudo btrfs subvolume sync /var/lib/mill; echo rc=$?; sudo btrfs subvolume list /var/lib/mill | grep e9; df -h /var/lib/mill; sudo btrfs filesystem du -s /var/lib/mill/cache-e9-target /var/lib/mill/run-e9-target /var/lib/mill/cache-e9-gen-2

real	0m0.043s
user	0m0.004s
sys	0m0.006s
rc=0
ID 397 gen 581 top level 256 path cache-e9-target
ID 399 gen 586 top level 256 path run-e9-target
ID 401 gen 584 top level 256 path cache-e9-gen-2
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.7G  928G   1% /var/lib/mill
     Total   Exclusive  Set shared  Filename
   2.61GiB       0.00B   792.20MiB  /var/lib/mill/cache-e9-target
   2.89GiB       0.00B   864.84MiB  /var/lib/mill/run-e9-target
   2.89GiB       0.00B   864.84MiB  /var/lib/mill/cache-e9-gen-2
```

(The `subvolume list` still showing gen-1 was taken between the delete and
the cleaner's pass; the sync's listing after it does not.) `df` did not move
on gen-1's deletion, and could not: every extent of gen-1 is shared with
`cache-e9-target`, the run's copy and gen-2 — `Exclusive 0.00B` all round —
so nothing was freed. The work order's "free space moves only after
`subvolume sync`" needs data only one subvolume holds. A writable snapshot
of gen-2 with 512M of `/dev/urandom` in it, deleted three ways:

```
$ btrfs subvolume snapshot /var/lib/mill/cache-e9-gen-2 /var/lib/mill/run-e9-scratch && dd if=/dev/urandom of=/var/lib/mill/run-e9-scratch/unique bs=1M count=512 status=none && sync && echo "before delete: $(df -h --output=used,avail /var/lib/mill | tail -1)"; btrfs subvolume delete /var/lib/mill/run-e9-scratch; echo rc=$?; echo "after delete:  $(df -h --output=used,avail /var/lib/mill | tail -1)"; time sudo btrfs subvolume sync /var/lib/mill; echo "after sync:    $(df -h --output=used,avail /var/lib/mill | tail -1)"
Create snapshot of '/var/lib/mill/cache-e9-gen-2' in '/var/lib/mill/run-e9-scratch'
before delete:  2.2G  928G
Delete subvolume 402 (no-commit): '/var/lib/mill/run-e9-scratch'
rc=0
after delete:   2.2G  928G
Waiting for 1 subvolume
Subvolume id 402 is gone (1/1)

real	0m1.026s
user	0m0.006s
sys	0m0.004s
after sync:     2.2G  928G
$ sleep 5; df -h --output=used,avail /var/lib/mill | tail -1; sync; sleep 30; df -h --output=used,avail /var/lib/mill | tail -1
 2.2G  928G
 1.7G  928G
$ btrfs subvolume snapshot /var/lib/mill/cache-e9-gen-2 /var/lib/mill/run-e9-scratch && dd if=/dev/urandom of=/var/lib/mill/run-e9-scratch/unique bs=1M count=512 status=none && sync && echo "before delete:       $(df -h --output=used /var/lib/mill | tail -1)"; btrfs subvolume delete /var/lib/mill/run-e9-scratch >/dev/null; echo "after delete:        $(df -h --output=used /var/lib/mill | tail -1)"; sudo btrfs subvolume sync /var/lib/mill >/dev/null; echo "after subvol sync:   $(df -h --output=used /var/lib/mill | tail -1)"; sync; echo "after sync(1):       $(df -h --output=used /var/lib/mill | tail -1)"; btrfs filesystem sync /var/lib/mill; echo "after fs sync:       $(df -h --output=used /var/lib/mill | tail -1)"
Create snapshot of '/var/lib/mill/cache-e9-gen-2' in '/var/lib/mill/run-e9-scratch'
before delete:        2.2G
after delete:         2.2G
after subvol sync:    2.2G
after sync(1):        1.7G
after fs sync:        1.7G
$ btrfs subvolume snapshot /var/lib/mill/cache-e9-gen-2 /var/lib/mill/run-e9-scratch >/dev/null && dd if=/dev/urandom of=/var/lib/mill/run-e9-scratch/unique bs=1M count=512 status=none && sync && echo "before delete:            $(df -h --output=used /var/lib/mill | tail -1)"; id -u; time btrfs subvolume delete --commit-after /var/lib/mill/run-e9-scratch; echo rc=$?; echo "after delete --commit-after: $(df -h --output=used /var/lib/mill | tail -1)"; for i in 1 2 3 4 5 6; do sleep 5; echo "+$((i*5))s: $(df -h --output=used /var/lib/mill | tail -1)"; done; btrfs filesystem sync /var/lib/mill; echo rc=$?; echo "after fs sync:            $(df -h --output=used /var/lib/mill | tail -1)"
before delete:             2.2G
1000
Delete subvolume 404 (commit): '/var/lib/mill/run-e9-scratch'

real	0m0.026s
user	0m0.000s
sys	0m0.025s
rc=0
after delete --commit-after:  2.2G
+5s:  2.2G
+10s:  2.2G
+15s:  2.2G
+20s:  2.2G
+25s:  2.2G
+30s:  2.2G
rc=0
after fs sync:             1.7G
```

So: `df` does not move on `delete`, nor on `--commit-after`, nor for 30 s
of waiting, nor on `subvolume sync` reporting the subvolume gone; it moves
when the next transaction commits, which `sync` or `btrfs filesystem sync`
forces — and `btrfs filesystem sync`, unlike `subvolume sync`, ran as the
operator (`rc=0`, uid 1000). The unprivileged recipe for §Eviction is
therefore `btrfs subvolume delete`, then `btrfs filesystem sync`, then
`statfs`.

```
$ time sudo systemctl stop mill-run-e9; sudo machinectl list --no-pager | grep -c mill-e9; sudo ausearch -m avc -ts 06:27:00 2>&1 | tail -2

real	0m0.231s
user	0m0.006s
sys	0m0.005s
0
<no matches>
```

**Verdict: E9 passes. Restore is 1 ms and promote 52 ms for a 2.8G / 3.0G lineage (`du -s` estimate 2 834 100 KiB); `1f/1fdbddf5…-d` is in gen-2 and the run's copy and not in gen-1; deleting gen-1 under the live run changed nothing inside it; the bind needed only `:idmap`. Every `create`, `snapshot`, `snapshot -r`, `delete` and `filesystem sync` ran as the operator without `sudo`. Two commands did not: deleting a read-only snapshot fails with `EROFS` until its owner clears `ro` with `btrfs property set`, and `btrfs subvolume sync` (like `show` and `list`) needs root and is not needed — free space follows a transaction commit, which `btrfs filesystem sync` forces unprivileged.**

---

## E10 — Derived image: build, export, boot, and the fallback

Run 2026-09-11 06:33 – 06:39 PDT. The pipeline is
`spikes/nspawn/build-derived.sh`: digest from `sha256sum <<<"<base> <packages>"`,
one `flock` per digest in `/var/lib/mill`, `podman build` from a generated
Containerfile, `podman export | tar` into `mill-image-<digest>.building`,
the E2 `setcap` pair, `mv` into place; a `trap` removes `.building` (and the
export container) on any failure. `/var/lib/machines` is root's, so the
subvolume steps are `sudo`; the lock is the operator's.

### First form, and the AVC it caused

The first form of the script was the work order's — the whole body inside
`flock <file> sh -c '…'`. It built, blocked and fell back correctly, but
every build logged one denial:

```
type=AVC msg=audit(1789133660.280:126227): avc:  denied  { read } for  pid=3855857 comm="pasta" path="/var/lib/mill/image-599b7919d9a115b9.lock" dev="dm-2" ino=2790 scontext=unconfined_u:unconfined_r:pasta_t:s0-s0:c0.c1023 tcontext=unconfined_u:object_r:mill_var_lib_t:s0 tclass=file permissive=0
```

`flock <file>` opens the lock on fd 3 and the command inherits it; podman
hands every open fd to `pasta`, its rootless network helper, which is
confined (`pasta_t`) and may not read `mill_var_lib_t`. Harmless — pasta
never wanted the file — but a denial per build is noise `homestead#35`
would have to explain. The committed script holds the lock on fd 9 and
closes it for each podman (`9<&-`); six denials from the first form, none
after it (`ausearch -m avc -ts 06:37:37`: `<no matches>`). The runs below
are the committed script's.

### Two builds of one digest, together

`[A]` starts first; `[B]` two seconds later; four seconds in, `ps` shows
who holds and who waits.

```
$ digest=$(sha256sum <<<"mill-base-spike libpq-devel" | cut -c1-16); echo "digest=$digest"; ls -l /var/lib/mill/image-*.lock 2>&1; sudo ls /var/lib/machines
digest=599b7919d9a115b9
ls: cannot access '/var/lib/mill/image-*.lock': No such file or directory
mill-image-e8
mill-image-e8.tpmstate
mill-image-spike
mill-image-spike.tpmstate
$ cd /home/ivy/src/github.com/ivy/mill; ( spikes/nspawn/build-derived.sh mill-base-spike libpq-devel 2>&1 | sed "s/^/[A] /" ) & sleep 2; ( echo "[B] starting $(date +%T.%N | cut -c1-12)"; spikes/nspawn/build-derived.sh mill-base-spike libpq-devel 2>&1 | sed "s/^/[B] /"; echo "[B] done $(date +%T.%N | cut -c1-12)" ) & sleep 2; echo "--- 4 s in: who holds and who waits"; ps -o pid,etimes,stat,wchan:16,comm -C flock; sudo lsof /var/lib/mill/image-599b7919d9a115b9.lock | tail -n +2 | awk "{print \$1, \$2, \$4}"; wait; echo "[done $(date +%T.%N | cut -c1-12)]"
[B] starting 06:37:39.789
--- 4 s in: who holds and who waits
    PID ELAPSED STAT WCHAN            COMMAND
3865800       2 S    locks_lock_inode flock
bash 3865584 9w
bash 3865795 9w
flock 3865800 9w
[A] built: /var/lib/machines/mill-image-599b7919d9a115b9 (build 4.282404082s, export .837798632s)
[B] present: /var/lib/machines/mill-image-599b7919d9a115b9
[B] done 06:37:42.986
[done 06:37:42.986]
```

`[A]`'s `flock 9` has returned (its `bash 3865584` holds fd 9 and is in the
build); `[B]`'s `flock` (`3865800`) is asleep in `locks_lock_inode`, waiting
on the same inode. `[A]` builds in 4.3 s and exports in 0.8 s; `[B]` wakes,
finds the subvolume and prints `present` at 06:37:42.99, 3.2 s after it
started — it built nothing. (The 4 s build is `dnf` with warm repository
metadata; the base spike's setup measured 18.7 s for the base's much longer
package list including the registry pull. A first derived build on a cold
`dnf` cache pays the metadata download, ~50 MB here, on top.)

### Boot from the derived subvolume

```
$ sudo systemd-run --unit=mill-run-e10 --collect -p KillMode=mixed systemd-nspawn --boot --quiet --machine=mill-e10 --directory=/var/lib/machines/mill-image-599b7919d9a115b9 --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false; until sudo systemd-run -M mill-e10 --pipe --wait --quiet --collect -- true 2>/dev/null; do sleep 0.2; done; sudo systemd-run -M mill-e10 --pipe --wait --quiet --collect -- systemctl is-system-running --wait; sudo systemd-run -M mill-e10 --pipe --wait --quiet --collect -p User=agent -- sh -c "id -un; rpm -q libpq-devel; ls /usr/include/libpq-fe.h"; echo rc=$?; sudo systemctl stop mill-run-e10; sudo machinectl list --no-pager | grep -c mill-e10
Running as unit: mill-run-e10.service; invocation ID: a10709f6785343f3b8054935303b683f
running
agent
libpq-devel-18.0-4.fc44.x86_64
/usr/include/libpq-fe.h
rc=0
0
```

(The first form's boot also checked `pkg-config --modversion libpq` →
`18beta1` and `getcap /usr/bin/newuidmap` → `cap_setuid=ep` inside: the
export's `setcap` survives the boot.)

### The size of a flat export

```
$ sudo du -sh /var/lib/machines/mill-image-spike /var/lib/machines/mill-image-599b7919d9a115b9; sudo du -sm /var/lib/machines/mill-image-spike/usr /var/lib/machines/mill-image-599b7919d9a115b9/usr /var/lib/machines/mill-image-spike/var/log /var/lib/machines/mill-image-599b7919d9a115b9/var/log; sudo btrfs filesystem du -s /var/lib/machines/mill-image-spike /var/lib/machines/mill-image-599b7919d9a115b9; podman image inspect -f "{{.Size}}" mill-base-spike mill-derived-599b7919d9a115b9
1020M	/var/lib/machines/mill-image-spike
1005M	/var/lib/machines/mill-image-599b7919d9a115b9
982	/var/lib/machines/mill-image-spike/usr
983	/var/lib/machines/mill-image-599b7919d9a115b9/usr
18	/var/lib/machines/mill-image-spike/var/log
2	/var/lib/machines/mill-image-599b7919d9a115b9/var/log
     Total   Exclusive  Set shared  Filename
 983.61MiB       0.00B   589.77MiB  /var/lib/machines/mill-image-spike
 968.56MiB   968.56MiB       0.00B  /var/lib/machines/mill-image-599b7919d9a115b9
1033579598
1052349636
```

The derived subvolume is **1005M against the base's 1020M** — base plus
the package, where the package is 1 MB of `/usr` and podman's layer for it
19 MB (`1052349636 − 1033579598`, the rpm database rewritten). The base
subvolume is 15 MB *larger* because somebody booted it non-ephemerally at
some point and it carries 18 MB of journal under `/var/log`; a fresh export
of the base would be ~1005M too. The flat-export cost §Images defers is
therefore exactly what it says: a derived image is a whole second copy of
the base, ~1 GB, sharing no extents with it (`Exclusive 968.56MiB`, `Set
shared 0.00B`) — one gigabyte per distinct package list, on `/var/lib/mill`
with `compress=zstd:1`. Exporting into a snapshot of the base and
overlaying only the new layer would share the gigabyte, and is the
optimisation to reach for if the number of derived images ever matters.

### A package that does not exist

```
$ cd /home/ivy/src/github.com/ivy/mill; time spikes/nspawn/build-derived.sh mill-base-spike no-such-package-e10 2>/tmp/spike-out/e10/fail.stderr; echo rc=$?; echo "--- stderr:"; cat /tmp/spike-out/e10/fail.stderr; sudo ls /var/lib/machines; sudo btrfs subvolume list /var/lib/machines | grep -c building; podman images --format "{{.Repository}}" | grep mill-derived

real	0m4.354s
user	0m3.654s
sys	0m0.311s
rc=1
--- stderr:
build failed for mill-base-spike + no-such-package-e10; dnf said:
No match for argument: no-such-package-e10
You can try to add to command line:
  --skip-unavailable to skip unavailable packages
mill-image-599b7919d9a115b9
mill-image-e8
mill-image-e8.tpmstate
mill-image-spike
mill-image-spike.tpmstate
0
localhost/mill-derived-599b7919d9a115b9
```

`rc=1`, no `.building`, no image for the failed digest, and `dnf`'s last
three lines on stderr. The raw build log ends with podman's own line,
`Error: building at STEP "RUN dnf install -y no-such-package-e10 && dnf clean all": while running runtime: exit status 1`;
the script drops that one so the three lines are `dnf`'s — the first form
printed a literal `tail -3` and got two of dnf's and one of podman's, which
is not the finding a session can act on. For the record, dnf's full tail:

```
Failed to resolve the transaction:
No match for argument: no-such-package-e10
You can try to add to command line:
  --skip-unavailable to skip unavailable packages
```

A `dnf` failure happens before `.building` exists, so it does not exercise
the `trap`. A failure after it does, forced with a `podman` shim whose
`export` dies mid-stream:

```
$ cd /home/ivy/src/github.com/ivy/mill; PATH=/tmp/spike-out/e10/fakebin:$PATH spikes/nspawn/build-derived.sh mill-base-spike libpq-devel ncurses; echo rc=$?; sudo ls /var/lib/machines | grep -c building; podman ps -a --format "{{.Image}}" | grep -c mill-derived; podman rmi mill-derived-1de6411ca553779e >/dev/null
fake podman: export failed
rc=1
0
0
```

No `.building`, no leftover container. (The first form of the trap did
leave the `podman create`d container behind; the committed one removes it.)

**Verdict: E10 passes. `rpm -q libpq-devel` → `libpq-devel-18.0-4.fc44.x86_64` as `agent` in a machine booted from `mill-image-599b7919d9a115b9`; the second build slept in `locks_lock_inode` on the lock and printed `present` without building; the missing package fails with `rc=1`, no `.building`, and `No match for argument: no-such-package-e10` as the first of dnf's three lines. Build 4.3 s, export 0.8 s; the derived subvolume is 1005M against the base's 1020M, a whole unshared copy.**

### Teardown (E9–E10)

```
$ df -h /var/lib/mill; for s in /var/lib/mill/cache-e9-* /var/lib/mill/run-e9-*; do btrfs property set -ts "$s" ro false 2>/dev/null; btrfs subvolume delete "$s"; done; df -h /var/lib/mill; sudo btrfs subvolume sync /var/lib/mill; df -h /var/lib/mill; btrfs filesystem sync /var/lib/mill; df -h /var/lib/mill
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  2.3G  928G   1% /var/lib/mill
Delete subvolume 401 (no-commit): '/var/lib/mill/cache-e9-gen-2'
Delete subvolume 397 (no-commit): '/var/lib/mill/cache-e9-target'
Delete subvolume 399 (no-commit): '/var/lib/mill/run-e9-target'
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  2.3G  928G   1% /var/lib/mill
Waiting for 3 subvolumes
Subvolume id 397 is gone (1/3)
Subvolume id 399 is gone (2/3)
Subvolume id 401 is gone (3/3)
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  2.3G  928G   1% /var/lib/mill
Filesystem        Size  Used Avail Use% Mounted on
/dev/mapper/mill  932G  1.4G  929G   1% /var/lib/mill
$ sudo btrfs subvolume delete /var/lib/machines/mill-image-599b7919d9a115b9; sudo rm -f /var/lib/mill/image-*.lock; podman rmi mill-derived-599b7919d9a115b9 >/dev/null; rmdir /home/ivy/src/github.com/ivy/mill/.cache/go /home/ivy/src/github.com/ivy/mill/.cache; sudo btrfs subvolume list /var/lib/mill; sudo btrfs subvolume list /var/lib/machines; ls /var/lib/mill; sudo machinectl list --no-pager; systemctl list-units "mill-run-e*" --all --no-legend; ls -l /etc/systemd/system/mill-machine@.service; podman images --format "{{.Repository}}" | grep mill
Delete subvolume 410 (no-commit): '/var/lib/machines/mill-image-599b7919d9a115b9'
ID 256 gen 606 top level 5 path @mill
ID 257 gen 605 top level 5 path @machines
ID 298 gen 566 top level 257 path @machines/mill-image-spike
ID 395 gen 560 top level 256 path mill-image-e7
ID 396 gen 577 top level 257 path @machines/mill-image-e8
ID 256 gen 606 top level 5 path @mill
ID 257 gen 605 top level 5 path @machines
ID 298 gen 566 top level 257 path mill-image-spike
ID 395 gen 560 top level 256 path @mill/mill-image-e7
ID 396 gen 577 top level 257 path mill-image-e8
dagger
mill-image-e7
runs
MACHINE        CLASS SERVICE      OS VERSION ADDRESSES
qemu-1-omarchy vm    libvirt-qemu -  -       -

1 machines listed.
-rw-r--r--. 1 root root 1640 Sep 11 01:03 /etc/systemd/system/mill-machine@.service
localhost/mill-base-spike
```

The same picture once more, at teardown: 2.3G used before and after the
deletes and after `subvolume sync`, 1.4G after `filesystem sync`. No
`cache-e9-*`, `run-e9-*` or `mill-image-599b…` subvolume, no lock, no
`mill-e*` machine, no `mill-run-e*` unit. `mill-image-spike`,
`mill-image-e7`, `mill-image-e8`, `/var/lib/mill/{dagger,runs}` and
homestead's unit are other nodes' and untouched. The `.cache` mount point
created in the checkout for the bind is gone.

### What the design takes from E9–E10

- **§Caching.** The claims hold: restore 1 ms, promote 52 ms, for ~3 GB.
  A subvolume binds with `--bind=…:idmap` like any directory, and the
  operator's `btrfs subvolume create`/`snapshot`/`delete` need no `sudo`.
  The size estimate §Promote records (`du -s` of the read-only generation)
  is apparent size; on this `compress=zstd:1` filesystem a Go cache takes
  about half that on disk, so `mill#67` ranks victims by `du` but must
  expect `statfs` to move by less.
- **§Promote.** A generation made with `snapshot -r` cannot be deleted by
  its owner until `btrfs property set -ts <gen> ro false`; pruning to the
  two newest generations, and eviction, do that first. `mill#60` and
  `mill#67` own this.
- **§Eviction.** "`btrfs subvolume sync` before re-reading free space" is
  wrong twice over: it needs root, and it returns before free space moves.
  The unprivileged sequence that works is `btrfs subvolume delete`, then
  `btrfs filesystem sync <mountpoint>` (or `sync`), then `statfs`.
  `--commit-after` on the delete is not enough on its own.
- **§Images.** The build-export-rename under `flock` works as written, and
  `spikes/nspawn/build-derived.sh` is its shape for `mill#59`, with two
  additions: the lock fd is closed for podman (else `pasta_t` logs a denial
  per build), and the export's container is removed on failure. The `dnf`
  tail to attach is the three lines before podman's `Error: building at
  STEP`. A derived image is a full ~1 GB copy sharing nothing with the base;
  fine at a handful of package lists, and the thing to change (export onto a
  snapshot of the base) if `mill image ls` ever shows dozens.
- **§Network / `homestead#34`.** The squid that landed admits `localhost`
  only (`TCP_DENIED/403` from `10.99.255.1`); the `machines` ACL of
  `spikes/nspawn/squid.conf` is missing from `/etc/squid/squid.conf`. Until
  it is there no machine can fetch anything.
- **§Testing.** The four scenarios run live in
  `spikes/nspawn/cache_test.go` under the same `MILL_SANDBOX_HOST=1` gate:
  the lineage pair fills a 2.4G cache from this repository in a minute and
  measured restore 1.7 ms / promote 1.6 ms; the image pair drives
  `build-derived.sh` twice at once and once with the missing package.
- **Tooling.** `btrfs subvolume show`, `list` and `sync` all need
  `CAP_SYS_ADMIN`; the worker lists lineages and generations from the
  directory tree and its manifests, never from btrfs.

## E13 — nspawn inside nspawn

Run 2026-09-11 09:46 – 09:53 PDT on `core`. The image subvolume from the boot
spike was still present (`ID 298 … mill-image-spike`), so no rebuild. The host
network is now homestead's, not this spike's: `systemd-networkd` is active with
`70-mill-ve.network` (a `/28` per `ve-mill*` veth, DHCP, `DNS=10.99.255.1`,
`IPMasquerade=no`), squid listens on `:3128` with a `mill` ACL admitting
`192.168.0.0/16`, and `inet mill` is loaded from `/etc/nftables/mill.nft`
(`ve-mill*` admitted to firewalld's `mill` zone). So "start squid and load the
table again" was not needed and was not done; the spike's own `squid.conf` and
`egress.nft` stayed on disk. Homestead's `mill-machine@.service` was not
touched; the outer boots through `systemd-run` as E1 did.

The preflight:

```
$ findmnt -no OPTIONS /var/lib/machines | tr , '\n' | grep user_subvol_rm_allowed
user_subvol_rm_allowed
```

### The outer

Booted as written, plus the three things the boot and network spikes settled:
`:idmap` on the bind (E4), a fresh `proc` over the masked one (E2), and a
read-only bind of the host's `/sys` at `/.hostsys`, which the inner turns out
to need (below). `agent` gets `ALL=(ALL) NOPASSWD: ALL` through
`/etc/sudoers.d/` in the running machine — **that is this spike's shortcut for
what the `nested` image variant bakes; it is not the design** — and
`systemd-container` and `btrfs-progs`, which the spike image lacks, are
installed through the host proxy at the outer's gateway address.

```
$ sudo systemd-run --unit=mill-run-e13 --collect -p KillMode=mixed \
    systemd-nspawn --boot --quiet --machine=mill-e13 \
      --directory=/var/lib/machines/mill-image-spike --ephemeral \
      --private-users=1048576:1048576 --private-users-ownership=auto \
      --network-veth --bind-ro=/var/lib/machines:/var/lib/machines:idmap \
      --bind-ro=/sys:/.hostsys --settings=false
Running as unit: mill-run-e13.service; invocation ID: 90dd0b0657e145d3bbbdc24f8cd29290
$ M() { sudo systemd-run -M mill-e13 --pipe --wait --quiet --collect "$@"; }
$ M -- systemctl is-system-running --wait
running
$ M -- mount -t proc -o nosuid,nodev,noexec proc /proc
$ M -- sh -c 'echo "agent ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/agent-spike-e13 && chmod 0440 /etc/sudoers.d/agent-spike-e13'
$ M -- networkctl status host0 --no-pager | awk '/Gateway:/{print $2}'
192.168.203.209
$ M -p User=agent -- sudo dnf install -y -q --setopt=proxy=http://192.168.203.209:3128 systemd-container btrfs-progs iproute
[…44 packages…]
[44/44] Installing systemd-container-0: 100% |   7.0 MiB/s |   2.3 MiB |  00m00s
$ sudo machinectl list --no-pager
MACHINE        CLASS     SERVICE        OS     VERSION ADDRESSES
mill-e13       container systemd-nspawn fedora 44      192.168.203.218…
```

Boot to `dnf` done: 9.1 s.

### The inner, as written: three refusals

Ephemeral from the read-only bind, exactly as the work order has it:

```
$ M -p User=agent -- sudo systemd-run --unit=inner --collect \
    systemd-nspawn --boot --quiet --machine=inner --directory=/var/lib/machines/mill-image-spike --ephemeral \
      --private-users=pick --network-veth --settings=false
Running as unit: inner.service; invocation ID: 2cfc64fb0b724f4e9838ce07b9ffa1a8
$ M -- journalctl -u inner --no-pager -o cat | tail -3
Failed to lock /var/lib/machines/.#machine.mill-image-spikeeb7712c8f3eb1df3: Read-only file system
inner.service: Main process exited, code=exited, status=1/FAILURE
inner.service: Failed with result 'exit-code'.
```

`--ephemeral` snapshots *next to* the source and needs the source's parent
writable, so a read-only bind of `/var/lib/machines` can never be the inner's
`--directory`. The bind can stay read-only; the outer snapshots the image into
its own tree first (its root is a btrfs subvolume on the same filesystem, so
the snapshot is a reflink, not a copy):

```
$ M -- sh -c 'mkdir -p /var/lib/mill-machines && btrfs subvolume snapshot /var/lib/machines/mill-image-spike /var/lib/mill-machines/mill-image-spike'
Create snapshot of '/var/lib/machines/mill-image-spike' in '/var/lib/mill-machines/mill-image-spike'
```

From that, `--private-users=pick` picks a base with no regard to the outer's
own map and then cannot idmap the tree; `chown` ownership fails on the first
`fchown` to nspawn's busy base `0xFFFE0000`, which is outside any outer range:

```
$ M -- sh -c 'timeout 30 systemd-nspawn --boot --machine=inner --directory=/var/lib/mill-machines/mill-image-spike --ephemeral --private-users=pick --network-veth --settings=false; echo "rc=$?"'
░ Spawning container inner on /var/lib/mill-machines/.#machine.mill-image-spike5f645341a082b0cb.
Selected user namespace base 1231486976 and range 65536.
Failed to set up ID mapped mounts: Operation not permitted
rc=1
$ M -- sh -c 'timeout 60 systemd-nspawn --boot --machine=inner --directory=/var/lib/mill-machines/mill-image-spike --ephemeral --private-users=65536:65536 --private-users-ownership=chown --network-veth --settings=false; echo "rc=$?"'
░ Spawning container inner on /var/lib/mill-machines/.#machine.mill-image-spikefefc296bf4b30ef4.
Selected user namespace base 65536 and range 65536.
Failed to adjust UID/GID shift of OS tree: Invalid argument
rc=1
```

(`--private-users-ownership=map` gives the same `Operation not permitted`: the
outer's root is already an idmapped mount and the kernel refuses to stack
another. The first attempt at this node traced both to `shift-uid.c`'s
`UID_BUSY_BASE` dance and nspawn's `pick` picking outside the map.)

`--private-users=identity` — a user namespace, mapped 1:1 onto the outer's —
needs no ownership change at all, and gets past the uid work to the next
refusal:

```
$ M -- sh -c 'timeout 30 systemd-nspawn --boot --machine=inner --directory=/var/lib/mill-machines/mill-image-spike --ephemeral --private-users=identity --network-veth --settings=false; echo "rc=$?"'
░ Spawning container inner on /var/lib/mill-machines/.#machine.mill-image-spike558cbfcf6d80b685.
Selected user namespace base 0 and range 65536.
Failed to mount proc (type proc) on /run/host/proc (MS_NOSUID|MS_NODEV|MS_NOEXEC ""): Operation not permitted
rc=1
```

This is E2's kernel rule one level up: a new `proc` (and `sysfs`) in a user
namespace is allowed only when the mount namespace already holds a fully
visible one, and nspawn has just pivoted into a tree that holds neither. The
fix is to have both inside the inner's tree when nspawn pivots: bind the
outer's own (unmasked, E2) `/proc` and the host's `/sys` (bound read-only into
the outer at boot as `/.hostsys`; a fresh `sysfs` mount in the outer needs
`LIBMOUNT_FORCE_MOUNT2=always` and is not fully visible anyway) to two paths in
the inner. With that, `--ephemeral` works from the outer-owned snapshot.

### The recipe, and the five checks

```
$ I() { M -p User=agent -- sudo systemd-run -M inner --pipe --wait --quiet --collect "$@"; }
$ M -p User=agent -- sudo systemd-run --unit=inner --collect \
    systemd-nspawn --boot --quiet --machine=inner --directory=/var/lib/mill-machines/mill-image-spike --ephemeral \
      --private-users=identity --network-veth --settings=false \
      --bind-ro=/proc:/.hostproc --bind-ro=/.hostsys:/.hostsys
Running as unit: inner.service; invocation ID: 59d5fc44c0eb45acb94215bd90309347
inner bus reachable after .338522596 s
$ I -- systemctl is-system-running --wait
running
inner is-system-running after 2.258797144 s
```

**Check 1 — the inner registers with the outer's machined:**

```
$ M -p User=agent -- sudo machinectl list --no-pager
MACHINE CLASS     SERVICE        OS     VERSION ADDRESSES
inner   container systemd-nspawn fedora 44      192.168.146.50…

1 machines listed.
```

**Check 2 — `-M inner` from the outer:**

```
$ I -- sh -c 'cat /proc/self/uid_map; systemd-detect-virt; cat /etc/machine-id'; echo "rc=$?"
         0          0      65536
systemd-nspawn
9597dc7a83014f748d81d1b93bb3caa5
rc=0
```

**Check 3 — podman inside the inner, as `agent`** (the E2 `/proc` line first,
the proxy in the environment; the three `network is unreachable` retries are
the inner's DHCP lease arriving after `is-system-running`, and the pull went
through Harbor as E11 says):

```
$ I -- mount -t proc -o nosuid,nodev,noexec proc /proc
$ I -p User=agent -p Environment=HTTPS_PROXY=http://192.168.164.97:3128 -p Environment=https_proxy=http://192.168.164.97:3128 -- podman run --rm quay.io/podman/hello
Trying to pull quay.io/podman/hello:latest...
time="2026-09-11T09:51:22-07:00" level=warning msg="Failed, retrying in 1s ... (1/3). Error: initializing source docker://quay.io/podman/hello:latest: pinging container registry harbor.tailcbcc0e.ts.net: Get \"https://harbor.tailcbcc0e.ts.net/v2/\": proxyconnect tcp: dial tcp 192.168.164.97:3128: connect: network is unreachable"
time="2026-09-11T09:51:23-07:00" level=warning msg="Failed, retrying in 1s ... (2/3). Error: initializing source docker://quay.io/podman/hello:latest: pinging container registry harbor.tailcbcc0e.ts.net: Get \"https://harbor.tailcbcc0e.ts.net/v2/\": proxyconnect tcp: dial tcp 192.168.164.97:3128: connect: network is unreachable"
time="2026-09-11T09:51:24-07:00" level=warning msg="Failed, retrying in 1s ... (3/3). Error: initializing source docker://quay.io/podman/hello:latest: pinging container registry harbor.tailcbcc0e.ts.net: Get \"https://harbor.tailcbcc0e.ts.net/v2/\": proxyconnect tcp: dial tcp 192.168.164.97:3128: connect: network is unreachable"
Getting image source signatures
Copying blob sha256:81df7ff16254ed9756e27c8de9ceb02a9568228fccadbf080f41cc5eb5118a44
Copying config sha256:5dd467fce50b56951185da365b5feee75409968cbab5767b9b59e325fb2ecbc0
Writing manifest to image destination
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
[…]
rc=0
$ I -p User=agent -- podman info --format '{{.Store.GraphDriverName}} {{.Host.CgroupManager}} {{.Host.OCIRuntime.Name}} {{.Host.IDMappings.UIDMap}}'
overlay systemd crun [{0 1000 1} {1 10000 55536}]
```

On the first (1048576-uid) outer, with the lease already there, the same pull
and run took 0.86 s.

**Check 4 — subvolumes as `agent` in the outer.** As written it fails, and
fails the same way for root in the outer and for the operator on the host,
because deleting a *read-only* subvolume under `user_subvol_rm_allowed` is
`EROFS` for everyone but `CAP_SYS_ADMIN` — E9's finding, not nesting's. (Also
`/tmp` in the machine is tmpfs; the round-trip has to be on the btrfs root.)
With E9's `ro false` first it passes:

```
$ M -p User=agent -- sh -c 'cd /home/agent && btrfs subvolume create s && btrfs subvolume snapshot -r s s-ro && btrfs subvolume delete s s-ro && echo ok'; echo "rc=$?"
Create subvolume './s'
Create readonly snapshot of 's' in 's-ro'
ERROR: Could not destroy subvolume/snapshot: Read-only file system
Delete subvolume 766 (no-commit): '/home/agent/s'
Delete subvolume 767 (no-commit): '/home/agent/s-ro'
rc=1
$ cd /var/lib/mill && btrfs subvolume create s && btrfs subvolume snapshot -r s s-ro && btrfs subvolume delete s s-ro && echo ok; echo "rc=$?"     # the operator, on the host
Create subvolume './s'
Create readonly snapshot of 's' in 's-ro'
ERROR: Could not destroy subvolume/snapshot: Read-only file system
Delete subvolume 772 (no-commit): '/var/lib/mill/s'
Delete subvolume 773 (no-commit): '/var/lib/mill/s-ro'
rc=1
$ M -p User=agent -- sh -c 'cd /home/agent && btrfs subvolume create s && btrfs subvolume snapshot -r s s-ro && btrfs property set -ts s-ro ro false && btrfs subvolume delete s s-ro && echo ok'; echo "rc=$?"
Create subvolume './s'
Create readonly snapshot of 's' in 's-ro'
Delete subvolume 779 (no-commit): '/home/agent/s'
Delete subvolume 780 (no-commit): '/home/agent/s-ro'
ok
rc=0
```

`user_subvol_rm_allowed` on the host mount was enough; the outer's root
inherits it (`findmnt` inside: `btrfs rw,noatime,idmapped,seclabel,…,user_subvol_rm_allowed`)
and nothing was remounted. `btrfs subvolume list` is `Operation not permitted`
inside the outer even as root, as E9 found on the host.

**Check 5 — the inner reaches the host's proxy** at the outer's gateway (the
host side of `ve-mill-e13`) and at the fixed `10.99.255.1`:

```
$ I -p User=agent -- curl -sS -o /dev/null -w 'http=%{http_code} rc=%{exitcode} remote=%{remote_ip}:%{remote_port} %{time_total}s\n' -x http://192.168.203.209:3128 https://example.com
http=200 rc=0 remote=192.168.203.209:3128 0.030535s
$ I -p User=agent -- curl -sS -m 5 -o /dev/null -w 'http=%{http_code} rc=%{exitcode}\n' -x http://10.99.255.1:3128 https://example.com
http=200 rc=0
$ sudo tail -1 /var/log/squid/access.log
1789145312.556     24 192.168.203.218 TCP_TUNNEL/200 6450 CONNECT example.com:443 - HIER_DIRECT/104.20.23.154 -
```

Squid sees the *outer's* address, because the inner's traffic is masqueraded
in the outer. Nothing was configured for that: the image's `systemd-networkd`
applies the stock `80-container-ve.network` to `ve-inner`, which carries
`IPMasquerade=both`, and networkd sets `ip_forward=1` and installs an
`io.systemd.nat` table with a `masquerade` for the inner's `/28`. Both are
required — removing either times out — and one sysctl plus one rule by hand
restores it without networkd, so no second squid:

```
$ M -- sh -c 'nft delete table ip io.systemd.nat; nft list tables; echo "ip_forward=$(cat /proc/sys/net/ipv4/ip_forward)"'
ip_forward=1
$ I -p User=agent -- curl -sS -m 5 -o /dev/null -w 'http=%{http_code}\n' -x http://192.168.203.209:3128 https://example.com; echo "rc=$?"
curl: (28) Connection timed out after 5003 milliseconds
http=000
rc=28
$ M -- sh -c 'nft add table ip mill-inner; nft "add chain ip mill-inner postrouting { type nat hook postrouting priority srcnat; policy accept; }"; nft add rule ip mill-inner postrouting iifname ve-inner oifname host0 masquerade'
$ I -p User=agent -- curl -sS -m 5 -o /dev/null -w 'http=%{http_code} rc=%{exitcode}\n' -x http://192.168.203.209:3128 https://example.com
http=200 rc=0
$ M -- sh -c 'echo 0 > /proc/sys/net/ipv4/ip_forward'
$ I -p User=agent -- curl -sS -m 5 -o /dev/null -w 'http=%{http_code}\n' -x http://192.168.203.209:3128 https://example.com; echo "rc=$?"
curl: (28) Connection timed out after 5002 milliseconds
http=000
rc=28
$ M -- sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
$ I -p User=agent -- curl -sS -m 5 -o /dev/null -w 'http=%{http_code}\n' -x http://192.168.203.209:3128 https://example.com
http=200
```

The host's `inet mill` table is unchanged by this: the outer's masqueraded
packets arrive on `ve-mill-e13` from the outer's address, to `tcp/3128`, which
it already admits; no forwarding on the host is involved.

### The uid range

The work order's `--private-users=1048576:1048576` was tried first and every
check passed under it; then the outer was rebooted with
`--private-users=1048576:65536` and all five were re-run and passed.
Checks 1–4 above are transcribed from the 65536 run (gateway
`192.168.164.97`), check 5 and the masquerade probes from the 1048576 run
(gateway `192.168.203.209`); each check passed identically under both. The inner needs no range of its own beyond
the outer's, because `identity` is the only `--private-users` mode that works
one level down: `pick` and any explicit shift need either an idmapped mount
the kernel refuses to stack or a `chown` to `0xFFFE0000` the map cannot hold.
The inner's root is therefore the outer's root, host uid `1048576`, and the
inner's `agent` is host `1048576+1000`:

```
$ cat /proc/$(sudo machinectl show mill-e13 -p Leader --value)/uid_map        # 65536 run
         0    1048576      65536
$ I -- cat /proc/self/uid_map
         0          0      65536
$ M -- cat /etc/subuid
agent:10000:55536
```

**A 65536-uid outer is enough.** The wide range §Provision specifies for
`nested` buys nothing here; what nesting needs instead is `identity` on the
inner, which `[machine].nested` should carry into the inner nspawn argv.

### AVC denials

None at any level: `ausearch -m avc,user_avc -ts 09:46:00` on the host is
`<no matches>`; `journalctl -o cat | grep 'avc:  denied'` in the outer and in
the inner match only the `systemd-run` unit descriptions that ran the grep.

### Wall clock

| | |
|---|---|
| outer boot to `is-system-running` | 2.17 s |
| outer boot to `dnf` done and tree snapshotted | 9.1 s |
| inner boot to bus reachable | 0.34 s |
| inner boot to `is-system-running` | 2.26 s |
| podman hello in the inner, lease present | 0.86 s |

**Verdict: E13 pass, five of five**, with three changes to the written form:
the inner's `--directory` is a snapshot the outer takes of the read-only bind
(the bind itself stays read-only), the inner runs `--private-users=identity`,
and the inner's argv carries `--bind-ro=/proc:/.hostproc --bind-ro=/.hostsys:/.hostsys`
with the outer booted `--bind-ro=/sys:/.hostsys`. `[machine].nested` stands.

## E7 — Unprivileged nspawn for repos without `nested`

Run 2026-09-11 09:53 – 09:57 PDT, after E13 was stopped. Scoped as the work
order says: a non-nested machine, provisioned by the operator without `sudo`.

```
$ sudo systemctl enable --now systemd-nsresourced.socket systemd-mountfsd.socket
$ systemctl is-active systemd-nsresourced.socket systemd-mountfsd.socket
active
active
```

As written, from the root-`0700` `/var/lib/machines`:

```
$ systemd-run --user --unit=mill-run-e7 --collect \
    systemd-nspawn --boot --quiet --machine=mill-e7 \
      --directory=/var/lib/machines/mill-image-spike --ephemeral --private-users=pick --network-veth --settings=false
Running as unit: mill-run-e7.service; invocation ID: c6582f94acb540608dccc94a15bebc15
$ journalctl --user -u mill-run-e7 --no-pager -o cat | tail -4
Failed to pick version on path '/var/lib/machines/mill-image-spike': Permission denied
Failed to determine whether '/run/systemd/nspawn/propagate/mill-e7' is the root file system: Permission denied
mill-run-e7.service: Main process exited, code=exited, status=1/FAILURE
mill-run-e7.service: Failed with result 'exit-code'.
```

A copy the operator can read was already on the host from the first attempt at
this node, made with `sudo btrfs subvolume snapshot /var/lib/machines/mill-image-spike /var/lib/mill/mill-image-e7`
(`drwxr-xr-x. 1 root root … /var/lib/mill/mill-image-e7`; root-owned, world
readable). From it, `pick` refuses outright, and the only mode an unprivileged
caller has is `managed`, which asks `systemd-nsresourced` for the range:

```
$ systemd-nspawn --boot --quiet --machine=mill-e7 --directory=/var/lib/mill/mill-image-e7 --ephemeral --private-users=pick --network-veth --settings=false
Unprivileged operation requires managed user namespaces, as otherwise no UID range can be acquired.
Failed to determine whether '/run/systemd/nspawn/propagate/mill-e7' is the root file system: Permission denied
$ timeout 30 systemd-nspawn --boot --quiet --machine=mill-e7 --directory=/var/lib/mill/mill-image-e7 --ephemeral --private-users=managed --network-veth --settings=false; echo "rc=$?"
rc=124
$ machinectl list --no-pager | grep -c mill-e7
0
```

Nothing is printed and nothing is registered: nspawn sits in `sigsuspend`
waiting on its varlink call to nsresourced, which SELinux stops from reading
the caller's user namespace, once per attempt:

```
$ sudo ausearch -m avc -ts 09:53:00 | grep 'avc:'
type=AVC msg=audit(1789145630.709:138047): avc:  denied  { read } for  pid=1195768 comm="systemd-nsresou" path="user:[4026533005]" dev="nsfs" ino=4026533005 scontext=system_u:system_r:systemd_nsresourced_t:s0 tcontext=system_u:object_r:nsfs_t:s0 tclass=file permissive=0
type=AVC msg=audit(1789145693.505:138068): avc:  denied  { read } for  pid=1202009 comm="systemd-nsresou" path="user:[4026533005]" dev="nsfs" ino=4026533005 scontext=system_u:system_r:systemd_nsresourced_t:s0 tcontext=system_u:object_r:nsfs_t:s0 tclass=file permissive=0
$ sudo cat /proc/$(pgrep -u ivy -f 'machine=mill-e7' | head -1)/stack | head -2
[<0>] sigsuspend.isra.0+0x79/0xa0
[<0>] __x64_sys_rt_sigsuspend+0x6d/0x80
```

The fix is a host policy change — `semanage permissive -a systemd_nsresourced_t`
for the experiment, or an `allow systemd_nsresourced_t nsfs_t:file read`
module for real — and this node did not make one: it is a change to `core`'s
SELinux policy, which homestead owns, and it was not tried with the cap
running. Fedora's policy for `systemd-nsresourced` under `systemd 259` is
where the first attempt at this node stopped too.

**Verdict: E7 fail, zero of four.** The boot does not happen (it hangs on
nsresourced rather than erroring), so veth, `-M` entry and podman were not
reached. Machines stay behind the root-owned template unit, as `docs/sandbox.md`
§Privilege already says; the one fact to carry is that unprivileged nspawn on
`core` is a policy-module question before it is anything else, and the copy
the operator can boot from must be made with `sudo` (the source is `0700`).

## E8 — vmspawn boots the same tree

Run 2026-09-11 09:56 – 10:03 PDT, under a twenty-minute cap, and by the work
order's rule for this attempt: no initrd is built and no vsock plumbing is
added; the host kernel and initrd are what the tree gets.

```
$ rpm -q systemd-vmspawn
package systemd-vmspawn is not installed
$ rpm -qf /usr/bin/systemd-vmspawn
systemd-container-259.8-1.fc44.x86_64
```

No package to install: on Fedora 44 `systemd-vmspawn` ships in
`systemd-container`, and `qemu-kvm-core-10.2.2-1.fc44` (its backend) was
already on the host.

### `--directory`, with the host kernel

The tree has no kernel, so `--directory` needs `--linux=`; with the host's
kernel and initrd the VM starts, `virtiofsd` serves the tree, and the initrd
refuses the root vmspawn hands it:

```
$ sudo timeout 90 systemd-vmspawn --machine=mill-e8 --directory=/var/lib/machines/mill-image-spike --network-tap --console=read-only
░ Spawning VM mill-e8 on /var/lib/machines/mill-image-spike.
Failed to discover any boot entries.
Failed to locate UKI in directory type image, please specify one with --linux=.
$ sudo timeout 120 systemd-vmspawn --machine=mill-e8 --directory=/var/lib/machines/mill-image-spike --network-tap --console=read-only --linux=/boot/vmlinuz-7.1.13-200.fc44.x86_64 --initrd=/boot/initramfs-7.1.13-200.fc44.x86_64.img
Executing: /usr/libexec/virtiofsd --shared-dir /var/lib/machines/mill-image-spike --xattr --fd 3 --no-announce-submounts
[2026-09-11T16:56:44Z INFO  virtiofsd] Client connected, servicing requests
Booting initrd of Fedora Linux 44 (Server Edition) dracut-108-7.fc44 (Initramfs).
[    1.485018] dracut: FATAL: Don't know how to handle 'root=root'
[    1.485076] dracut: Refusing to continue
[    1.503255] systemd-shutdown[1]: Powering off.
$ sudo lsinitrd /boot/initramfs-7.1.13-200.fc44.x86_64.img | grep -c virtiofs
0
```

`root=root` is vmspawn's virtiofs root tag, which only an initrd with the
`virtiofs` module (or a UKI made for it) understands; Fedora's host-only
initrd has neither, and building one is out of scope. That is the
`--directory` record: **it does not boot the tree with the host initrd.**

### `--image`, a raw disk made with `systemd-repart`

The raw image: one GPT root partition, ext4, the tree copied in.
`systemd-repart` was the tool; `--make-ddi=portable` was not tried because a
portable DDI needs the same virtiofs-less boot path, and `virt-make-fs` is not
installed. The first run failed with the auto size — `Failed to copy … to
'/run/systemd/mount-root/': No space left on device` — and `SizeMinBytes=4G`
fixed it:

```
$ cat /tmp/e8-repart.d/10-root.conf
[Partition]
Type=root
Format=ext4
CopyFiles=/
Label=root
SizeMinBytes=4G
$ sudo systemd-repart --empty=create --size=auto --definitions=/tmp/e8-repart.d --root=/var/lib/machines/mill-image-spike --dry-run=no /var/lib/machines/mill-image-spike.raw
Syncing future partition 0 contents to disk.
Adding new partition 0 to partition table.
Writing new partition table.
All done.
-rw-r--r--. 1 root root 4296036352 Sep 11 09:57 /var/lib/machines/mill-image-spike.raw
```

2 s to make. Booting it with the host kernel found the root partition by
GPT auto-discovery but then waited without limit for the *host's* LUKS
volume, which the host-only initrd carries in its baked `etc/crypttab`:

```
$ sudo systemd-run --unit=mill-run-e8 --collect -p KillMode=mixed systemd-vmspawn --machine=mill-e8 --image=/var/lib/machines/mill-image-spike.raw --network-tap --console=read-only --linux=/boot/vmlinuz-7.1.13-200.fc44.x86_64 --initrd=/boot/initramfs-7.1.13-200.fc44.x86_64.img
[    1.317752] systemd[1]: Expecting device dev-disk-by\x2dpartuuid-c18d41e3\x2d15ee\x2d4136\x2d827f\x2d3cf6da78ab2b.device - /dev/disk/by-partuuid/c18d41e3-15ee-4136-827f-3cf6da78ab2b...
[    1.317934] systemd[1]: Expecting device dev-disk-by\x2duuid-3e7ec610\x2d9806\x2d4bc6\x2d8632\x2d2f17cf4f5467.device - /dev/disk/by-uuid/3e7ec610-9806-4bc6-8632-2f17cf4f5467...
[  OK  ] Found device dev-disk-by\x2dpartuu…8d41e3-15ee-4136-827f-3cf6da78ab2b.
M[***   ] Job dev-disk-by\x2duuid-3e7ec610\x2…start running (2min 8s / no limit)
$ sudo lsinitrd /boot/initramfs-7.1.13-200.fc44.x86_64.img | grep crypttab
-rw-r--r--   1 root     root          110 Apr 28 17:00 etc/crypttab
```

`rd.luks=0 rd.lvm=0` on the kernel command line (arguments after `--`) get
past that, and the tree boots as a VM to a getty in under ten seconds:

```
$ sudo systemctl stop mill-run-e8; sudo machinectl terminate mill-e8
$ sudo systemd-run --unit=mill-run-e8 --collect -p KillMode=mixed systemd-vmspawn --machine=mill-e8 --image=/var/lib/machines/mill-image-spike.raw --network-tap --console=read-only --linux=/boot/vmlinuz-7.1.13-200.fc44.x86_64 --initrd=/boot/initramfs-7.1.13-200.fc44.x86_64.img -- rd.luks=0 rd.lvm=0
Running as unit: mill-run-e8.service; invocation ID: 287ee8064fd5454a95d929a40868df16
[    0.232073] virtio_blk virtio5: [vda] 8390696 512-byte logical blocks (4.30 GB/4.00 GiB)
[  OK  ] Reached target initrd-switch-root.target - Switch Root.
Welcome to Fedora Linux 44 (Container Image)!
[    3.370057] systemd[1]: Started systemd-journald.service - Journal Service.
[    9.752783] fbcon: Taking over console
Fedora Linux 44 (Container Image)
Kernel 7.1.13-200.fc44.x86_64 on x86_64 (hvc0)
$ sudo machinectl list --no-pager
MACHINE        CLASS SERVICE         OS VERSION ADDRESSES
mill-e8        vm    systemd-vmspawn -  -       -
qemu-1-omarchy vm    libvirt-qemu    -  -       -
```

`-M` does not reach it. vmspawn's machine transport is ssh over vsock — the
VM is handed `ssh.ephemeral-authorized_keys-all` and
`systemd.unit-dropin.sshd-vsock@.service` as credentials — and the tree has
no `sshd`:

```
$ sudo systemd-run -M mill-e8 --pipe --wait --quiet --collect -- true; echo "-M rc=$?"
Failed to connect to system scope bus via machine transport: Input/output error
-M rc=1
$ sudo machinectl shell mill-e8 /bin/true
Failed to get shell PTY: Operation not supported
$ sudo ls /var/lib/machines/mill-image-spike/usr/sbin/sshd
ls: cannot access '/var/lib/machines/mill-image-spike/usr/sbin/sshd': No such file or directory
```

Adding `openssh-server` to the tree and the `vsock` modules to the initrd is
where the first attempt at this node spent its last hour; it is not repeated.

**Verdict: E8 fail** on the pass line as written — the tree boots as a VM
(image tool `systemd-repart`, 2 s to make; 9.8 s from `systemd-run` to the
guest's getty, with `rd.luks=0 rd.lvm=0` and the host kernel), but `-M` does
not reach it without an `sshd` in the image. Half way, and what the vmspawn
upgrade in `docs/sandbox.md` would cost is now concrete: a kernel or UKI the
image carries, an `openssh-server` in it, and `podman` untested.

## Teardown (E13, E7, E8)

```
$ sudo systemctl stop mill-run-e13; systemctl --user stop mill-run-e7; sudo systemctl stop mill-run-e8; sudo machinectl terminate mill-e8
$ sudo systemctl disable --now systemd-nsresourced.socket systemd-mountfsd.socket
Removed '/etc/systemd/system/sockets.target.wants/systemd-nsresourced.socket'.
Removed '/etc/systemd/system/sockets.target.wants/systemd-mountfsd.socket'.
$ sudo rm -f /var/lib/machines/mill-image-spike.raw; sudo rm -rf /var/lib/machines/mill-image-spike.raw.tpmstate
$ sudo btrfs subvolume delete /var/lib/mill/mill-image-e7; sudo btrfs subvolume delete /var/lib/machines/mill-image-e8     # the first attempt's copies
Delete subvolume 395 (no-commit): '/var/lib/mill/mill-image-e7'
Delete subvolume 396 (no-commit): '/var/lib/machines/mill-image-e8'
$ sudo machinectl list --no-pager
MACHINE        CLASS SERVICE      OS VERSION ADDRESSES
qemu-1-omarchy vm    libvirt-qemu -  -       -

1 machines listed.
$ systemctl is-enabled systemd-nsresourced.socket systemd-mountfsd.socket
disabled
disabled
$ sudo ls /var/lib/machines/
mill-image-3c9a43029a6b9cdd908374177645456633f20d06e6837e21e08bd08e75be2c02
mill-image-spike
```

Squid, `systemd-networkd` and the `inet mill` table are homestead's and were
not started or stopped by this node; nothing on `core`'s SELinux policy was
changed; the image subvolume stays. `qemu-1-omarchy` is not this node's.

### What the design takes from E13, E7 and E8

- **§Self-hosting stands: `[machine].nested` stays.** All five E13 checks
  pass, and the live tier can run inside the agent's own machine as a check
  step. Three corrections to §Provision, for `mill#66`: the inner is booted
  `--private-users=identity` (the only mode that works one level down —
  `pick` and every explicit shift fail on the idmap or on the `chown`), so
  the outer needs **no** wide range, `--private-users=<base>:65536` is
  enough; the outer's `nested` argv adds `--bind-ro=/sys:/.hostsys` and the
  inner's adds `--bind-ro=/proc:/.hostproc --bind-ro=/.hostsys:/.hostsys`,
  or the inner's nspawn cannot mount `proc`; and the inner's `--ephemeral`
  runs from a snapshot the outer takes of the read-only bind, not from the
  bind, because `--ephemeral` snapshots beside its source. The bind itself
  stays read-only as the design says. `agent`'s full `sudo` is what the
  `nested` image variant bakes; here it was a `sudoers.d` line in the
  running outer.
- **Inner egress needs forwarding and one masquerade in the outer, no
  second squid.** The stock `80-container-ve.network` in the image does both
  (`IPMasquerade=both` → `ip_forward=1` and `io.systemd.nat`), so nothing
  had to be configured; if the image ever ships a `.network` for `ve-*`
  without it, the fix is `net.ipv4.ip_forward=1` and
  `iifname ve-* oifname host0 masquerade`. The host proxy is reachable from
  the inner at the outer's gateway or at `10.99.255.1`, and the host's table
  sees only the outer's address.
- **`user_subvol_rm_allowed` on the host is enough for the outer's btrfs
  operations**, with E9's rule carried over: a read-only snapshot needs
  `ro false` before its owner can delete it.
- **No SELinux work for nesting.** Zero denials on the host, in the outer
  and in the inner, so §SELinux's "the spike decides" resolves to: no module,
  no context flags, for nested as for flat machines.
- **E7 stays a spike, and its first obstacle is policy.** `systemd-nsresourced`
  is denied `read` on `nsfs_t` under `core`'s policy, and nspawn hangs on it
  rather than failing. §Privilege's template-unit answer is unchanged.
- **§Testing.** The four scenarios run live in `spikes/nspawn/nested_test.go`
  under the same `MILL_SANDBOX_HOST=1` gate and pass on `core` (46 s): the
  outer boots once for the three nested scenarios, and the unprivileged one
  records which of boot, veth, entry and podman worked rather than asserting
  any of them, then checks §E7 carries a verdict with a count.
- **vmspawn is a bigger step than the ADR's "same tree".** The tree boots as
  a VM only with a kernel supplied from outside and `rd.luks=0 rd.lvm=0`
  against the host's initrd; a real upgrade needs a kernel or UKI in the
  image, `openssh-server` for `-M`, and a raw image per boot (`systemd-repart`,
  2 s, 4 GB).
