#!/usr/bin/env bash
# Build the spike base image and export it as the btrfs subvolume
# /var/lib/machines/mill-image-spike that the nspawn spike experiments boot.
# Run from the repository root; needs rootless podman and sudo.
set -euo pipefail

# The host's /etc/containers/policy.json rejects every registry it does not
# name, and registry.fedoraproject.org is not among them; the spike policy
# admits that one registry and nothing else.
podman build --signature-policy spikes/nspawn/policy-spike.json \
  -t mill-base-spike -f spikes/nspawn/Containerfile.spike spikes/nspawn
sudo btrfs subvolume create /var/lib/machines/mill-image-spike
c=$(podman create mill-base-spike) \
  && podman export "$c" | sudo tar -C /var/lib/machines/mill-image-spike -x \
  && podman rm "$c"

# podman export drops file capabilities; rootless podman inside the machine
# needs these two back.
sudo setcap cap_setuid=ep /var/lib/machines/mill-image-spike/usr/bin/newuidmap
sudo setcap cap_setgid=ep /var/lib/machines/mill-image-spike/usr/bin/newgidmap
