#!/usr/bin/bash
# Build step of the image stage: `bootc container lint`, fatal on every
# error and every warning it finds. The one tolerated outcome is the linter
# being unable to run at
# all under user-mode emulation, where the kernel interface it opens files
# with (openat2) does not exist and it stops with ENOSYS before looking at
# anything. That happens when a machine builds the image for another
# architecture; a builder of the image's own architecture, CI's included,
# runs the linter for real.
set -uo pipefail
out=$(bootc container lint --fatal-warnings 2>&1)
status=$?
printf '%s\n' "$out"
if [ "$status" -ne 0 ] && printf '%s' "$out" | grep -q 'Function not implemented'; then
  echo "WARNING: bootc container lint cannot run under emulation here and was SKIPPED" >&2
  exit 0
fi
exit "$status"
