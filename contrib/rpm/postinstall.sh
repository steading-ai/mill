#!/bin/sh
# rpm's post-install scriptlet; $1 is 1 on first install and 2 or more on an
# upgrade. The account and the runtime directory come from the fragments the
# package just laid down. Nothing starts the service: a fresh install has no
# database yet, and the distribution's preset says whether it is enabled.
systemd-sysusers mill.conf
systemd-tmpfiles --create mill.conf || :
if [ "$1" -eq 1 ]; then
  systemctl --no-reload preset mill.service >/dev/null 2>&1 || :
fi
systemctl daemon-reload >/dev/null 2>&1 || :
