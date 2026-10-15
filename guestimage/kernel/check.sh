#!/bin/sh
# The check stage of the Guest Images' kernel (guestimage/kernel/Containerfile).
# The Dagger module runs it again on the built image (guest-image-check).
#
# usage: check.sh ROOT ARCH VERSION
#
# ROOT is the image's file tree, ARCH its architecture, amd64 or arm64, and
# VERSION the kernel's, such as 6.1.128. It needs only a POSIX shell, od and
# grep, so it runs in alpine on the build machine. It reports every failure
# before it exits non-zero.
#
#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Images build SHALL run a check stage for each image
#/ that fails unless the image holds what this document requires of it.
set -u

root=${1:?usage: check.sh ROOT ARCH VERSION}
arch=${2:?usage: check.sh ROOT ARCH VERSION}
want=${3:?usage: check.sh ROOT ARCH VERSION}
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}
# bytes FILE OFFSET COUNT: COUNT bytes of FILE from OFFSET, as hex.
bytes() { od -An -tx1 -j "$2" -N "$3" "$1" | tr -d ' \n'; }

#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Images SHALL be a kernel image and a root filesystem
#/ image, each defined by one Containerfile in `guestimage/` and built for
#/ `linux/amd64` and `linux/arm64`.
case $arch in
amd64 | arm64) ok "the kernel image is built for linux/$arch" ;;
*) fail "the kernel image is built for $arch, not amd64 or arm64" ;;
esac

#= docs/requirements/13-guest-image.md#guest-kernel
#= type=test
#/ The Guest Images' kernel image SHALL hold Firecracker's CI
#/ kernel 6.1.128 for its architecture at `boot/vmlinux`, and no other
#/ file.
kernel=$root/boot/vmlinux
if [ ! -s "$kernel" ]; then
	fail "no kernel at boot/vmlinux"
else
	ok "the kernel is at boot/vmlinux"
	# The kernel's banner names its version.
	version=$(grep -aoE 'Linux version [0-9]+\.[0-9]+\.[0-9]+' "$kernel" | head -1)
	version=${version#Linux version }
	if [ "$version" = "$want" ]; then
		ok "boot/vmlinux is Linux $version"
	else
		fail "boot/vmlinux is Linux '$version', not $want"
	fi
fi
# A Pool names one file; anything beside it only makes the pull longer.
extra=$(find "$root" -type f ! -path "$kernel" | head -5)
if [ -n "$extra" ]; then
	fail "the image holds more than the kernel: $extra"
else
	ok "the image holds the kernel and nothing else"
fi

#= docs/requirements/13-guest-image.md#guest-kernel
#= type=test
#/ The Guest Images' kernel image SHALL hold an x86-64 ELF
#/ `vmlinux` on `linux/amd64`, and an uncompressed arm64 `Image` on
#/ `linux/arm64`.
if [ -s "$kernel" ]; then
	case $arch in
	amd64)
		# The ELF magic, 64-bit, little-endian, and e_machine 0x3e, x86-64.
		if [ "$(bytes "$kernel" 0 6)" = 7f454c460201 ] && [ "$(bytes "$kernel" 18 2)" = 3e00 ]; then
			ok "boot/vmlinux is an x86-64 ELF vmlinux"
		else
			fail "boot/vmlinux is not an x86-64 ELF vmlinux"
		fi
		;;
	arm64)
		# The arm64 Image header's magic, "ARM\x64", at offset 56.
		if [ "$(bytes "$kernel" 56 4)" = 41524d64 ]; then
			ok "boot/vmlinux is an uncompressed arm64 Image"
		else
			fail "boot/vmlinux is not an uncompressed arm64 Image"
		fi
		;;
	*) fail "no kernel format check for $arch" ;;
	esac
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures kernel check(s) failed" >&2
	exit 1
fi
echo "every kernel check passed"
