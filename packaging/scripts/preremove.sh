#!/bin/sh
# rpm's pre-uninstall scriptlet; $1 is 0 on removal and 1 on an upgrade. A
# removal stops the worker — which releases its leases and leaves its runs
# resumable — and an upgrade leaves it to postremove's restart.
if [ "$1" -eq 0 ]; then
  systemctl --no-reload disable --now mill.service >/dev/null 2>&1 || :
fi
