#!/bin/sh
# Installs lantern-box and runs it as a Lantern node, which gets its config
# from lantern-cloud and starts on boot.
#
#   curl -fsSL https://raw.githubusercontent.com/getlantern/lantern-box/main/install.sh | sh
#
# Optional environment:
#   LANTERN_USER_ID     your Lantern user ID, which can enroll this node
#   LANTERN_NODE_ARGS   more `lantern-box serve` flags, such as "--bandwidth-limit 10mbit".
#                       systemd splits them at whitespace and ignores quotes, so
#                       no value can contain a space.
#   LANTERN_BOX_VERSION a release tag such as v0.0.203 (default: the latest
#                       Lamplighter release, which the Homebrew formula names)
#
# On Linux it installs the release's .deb or .rpm, or else its tarball, and
# enables the lantern-node systemd service. On macOS it uses Homebrew.
set -eu

repo=getlantern/lantern-box

say() { printf '%s\n' "$*"; }
die() { printf 'lantern-box install: %s\n' "$*" >&2; exit 1; }
has() { command -v "$1" >/dev/null 2>&1; }

# Runs a command as root, through sudo when we aren't root already.
as_root() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	elif has sudo; then
		sudo "$@"
	else
		die "run this as root, or install sudo"
	fi
}

node_args() {
	args=${LANTERN_NODE_ARGS:-}
	if [ -n "${LANTERN_USER_ID:-}" ]; then
		args="--user-id $LANTERN_USER_ID${args:+ $args}"
	fi
	printf '%s' "$args"
}

install_macos() {
	has brew || die "install Homebrew from https://brew.sh first, then run this again"
	brew install getlantern/tap/lantern-box
	args=$(node_args)
	if [ -n "$args" ]; then
		say "brew services runs the node with default flags. To pass \"$args\", run instead:"
		say "  lantern-box serve --register $args"
		return
	fi
	brew services start lantern-box
	say "The Lantern node is running. Check it with: brew services info lantern-box"
}

latest_tag() {
	# Lamplighter releases are the ones the Homebrew formula names, so Linux
	# and Homebrew installs get the same version. GitHub's latest release is
	# the proxy fleet's, which can differ.
	formula="https://raw.githubusercontent.com/getlantern/homebrew-tap/main/Formula/lantern-box.rb"
	text=$(curl -fsSL "$formula") ||
		die "couldn't read the latest Lamplighter release from $formula. Set LANTERN_BOX_VERSION to a release tag."
	version=$(printf '%s\n' "$text" | sed -n 's/^  version "\([0-9][0-9.]*\)"$/\1/p' | head -n 1)
	[ -n "$version" ] || die "couldn't find a version in $formula. Set LANTERN_BOX_VERSION to a release tag."
	printf 'v%s' "$version"
}

install_linux() {
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU $(uname -m): lantern-box ships for amd64 and arm64" ;;
	esac
	has curl || die "install curl first"
	manual="Download a release from https://github.com/$repo/releases and run: lantern-box serve --register"
	has systemctl || die "this installer needs systemd. $manual"
	# The lantern-node unit's DynamicUser and StateDirectory need systemd 235.
	systemd=$(systemctl --version | awk 'NR == 1 { print $2 }')
	case $systemd in
	'' | *[!0-9]*) die "couldn't read the systemd version. $manual" ;;
	esac
	[ "$systemd" -ge 235 ] || die "this installer needs systemd 235 or later, not $systemd. $manual"

	tag=${LANTERN_BOX_VERSION:-$(latest_tag)}
	version=${tag#v}
	base="https://github.com/$repo/releases/download/$tag"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	if has dpkg && has apt-get; then
		pkg=lantern-box_${version}_linux_$arch.deb
	elif has rpm && { has dnf || has yum || has zypper; }; then
		pkg=lantern-box_${version}_linux_$arch.rpm
	else
		pkg=lantern-box_${version}_linux_$arch.tar.gz
	fi
	say "Downloading lantern-box $tag ($pkg)"
	curl -fsSL -o "$tmp/$pkg" "$base/$pkg" || die "couldn't download $base/$pkg"
	curl -fsSL -o "$tmp/checksums.txt" "$base/lantern-box_${version}_checksums.txt" ||
		die "couldn't download the release's checksums"
	want=$(awk -v f="$pkg" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || die "$pkg isn't in the release's checksums"
	if has sha256sum; then
		got=$(sha256sum "$tmp/$pkg" | awk '{ print $1 }')
	else
		got=$(shasum -a 256 "$tmp/$pkg" | awk '{ print $1 }')
	fi
	[ "$got" = "$want" ] || die "$pkg doesn't match the release's checksum"

	case $pkg in
	# Downgrades are allowed so LANTERN_BOX_VERSION can pin an older release.
	*.deb) as_root apt-get install -y --allow-downgrades "$tmp/$pkg" ;;
	# The package has no dependencies, so rpm needs no package manager. It's
	# unsigned, so rpm can't check a signature, which newer Fedora enforces,
	# but it matched the release's checksum above.
	*.rpm) as_root rpm -U --oldpackage --replacepkgs --nosignature "$tmp/$pkg" ;;
	*)
		tar -xzf "$tmp/$pkg" -C "$tmp"
		as_root install -m 0755 "$tmp/lantern-box" /usr/bin/lantern-box
		as_root install -m 0644 "$tmp/lantern-node.service" /etc/systemd/system/lantern-node.service
		;;
	esac

	args=$(node_args)
	if [ -n "$args" ]; then
		as_root mkdir -p /etc/lantern-box
		# Escaped for the double-quoted value systemd's EnvironmentFile reads.
		escaped=$(printf '%s' "$args" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')
		printf 'LANTERN_NODE_ARGS="%s"\n' "$escaped" | as_root tee /etc/lantern-box/node.env >/dev/null
	fi
	as_root systemctl daemon-reload
	as_root systemctl enable lantern-node.service
	as_root systemctl restart lantern-node.service
	say "The Lantern node is running. Check it with: systemctl status lantern-node"
	say "Its logs: journalctl -u lantern-node -f"
}

case $(uname -s) in
Darwin) install_macos ;;
Linux) install_linux ;;
*) die "unsupported OS $(uname -s): see https://github.com/$repo#run-a-lantern-node" ;;
esac
