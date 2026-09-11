#!/usr/bin/env bash
# Build a derived image — the base plus a package list — and export it as the
# btrfs subvolume /var/lib/machines/mill-image-<digest> that a run boots:
# docs/sandbox.md §Images. This is the E10 pipeline of the nspawn spike, the
# starting point for mill#59.
#
#   build-derived.sh <base-image> <package>...
#
# The digest names the (base, packages) pair, so two runs wanting the same
# image want the same subvolume. Every step runs under one lock per digest:
# a second builder blocks, then finds the image present and exits. The
# export lands in <target>.building and is renamed into place last, so a
# subvolume that exists is complete, and a failed build removes its own
# .building and leaves the base for the run to boot. On failure the last
# three lines of dnf's output are the finding, on stderr.
#
# Needs rootless podman for the build and sudo for /var/lib/machines, which
# is root's; the lock lives in /var/lib/mill, which is the operator's.
set -euo pipefail

if [ $# -lt 2 ]; then
  echo "usage: $0 <base-image> <package>..." >&2
  exit 64
fi
base=$1; shift
packages="$*"
digest=$(sha256sum <<<"$base $packages" | cut -c1-16)
target=/var/lib/machines/mill-image-$digest
building=$target.building
tag=mill-derived-$digest

# The lock is held on fd 9 for the rest of the script. podman gets the fd
# closed: its network helper inherits every open fd, and SELinux logs a
# denial for a read of the lock it never needed.
exec 9>"/var/lib/mill/image-$digest.lock"
flock 9

if sudo test -d "$target"; then
  echo "present: $target"
  exit 0
fi

ctx=$(mktemp -d)
log=$(mktemp)
c=
cleanup() {
  rm -rf "$ctx" "$log"
  [ -z "$c" ] || podman rm -f "$c" >/dev/null 2>&1 || true
  if sudo test -d "$building"; then
    sudo btrfs subvolume delete "$building" >/dev/null
  fi
}
trap cleanup EXIT

t0=$(date +%s.%N)
if ! podman build -t "$tag" -f - "$ctx" >"$log" 2>&1 9<&- <<CONTAINERFILE
FROM $base
RUN dnf install -y $packages && dnf clean all
CONTAINERFILE
then
  # podman appends its own "Error: building at STEP" line to the log; the
  # three before it are from dnf.
  echo "build failed for $base + $packages; dnf said:" >&2
  grep -v "^Error: building at STEP" "$log" | tail -n 3 >&2
  exit 1
fi
t1=$(date +%s.%N)

sudo btrfs subvolume create "$building" >/dev/null
c=$(podman create "$tag" 9<&-)
podman export "$c" 9<&- | sudo tar -C "$building" -x
# podman export drops file capabilities; rootless podman inside the machine
# needs these two back (spike E2).
sudo setcap cap_setuid=ep "$building/usr/bin/newuidmap"
sudo setcap cap_setgid=ep "$building/usr/bin/newgidmap"
sudo mv "$building" "$target"
t2=$(date +%s.%N)

echo "built: $target (build $(echo "$t1 - $t0" | bc)s, export $(echo "$t2 - $t1" | bc)s)"
