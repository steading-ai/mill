#!/bin/sh
# rpm's post-uninstall scriptlet; $1 is 0 on removal and 1 or more on an
# upgrade. An upgrade restarts a running service onto the new binary; a
# service that was not running is left that way.
systemctl daemon-reload >/dev/null 2>&1 || :
if [ "$1" -ge 1 ]; then
  systemctl try-restart mill.service >/dev/null 2>&1 || :
fi
