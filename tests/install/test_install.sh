#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/runq-installer-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

release_dir="${tmp_dir}/releases/vtest"
mkdir -p "$release_dir"

write_checksum() {
  file=$1
  output=$2
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" > "$output"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" > "$output"
  else
    printf 'sha256sum or shasum is required for installer tests\n' >&2
    exit 1
  fi
}

make_asset() {
  name=$1
  marker=$2
  cat > "${release_dir}/${name}" <<EOF
#!/bin/sh
printf '%s\\n' '${marker}'
EOF
  chmod +x "${release_dir}/${name}"
  write_checksum "${release_dir}/${name}" "${release_dir}/${name}.sha256"
}

make_asset runq-linux-amd64 core-linux
make_asset runq-dashboard-linux-amd64 ui-linux
make_asset runq-darwin-arm64 core-darwin
make_asset runq-dashboard-darwin-arm64 ui-darwin

api_dir="${tmp_dir}/api/releases"
mkdir -p "$api_dir"
printf '%s\n' '{"tag_name":"vtest"}' > "${api_dir}/latest"

# The default path resolves the stable latest-release endpoint, installs only
# the core client, and has no daemon/data-directory side effects.
linux_bin="${tmp_dir}/linux-bin"
side_effect_dir="${tmp_dir}/must-not-exist"
unset RUNQ_VERSION RUNQ_START_DAEMON
RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
RUNQ_API_BASE="file://${tmp_dir}/api" \
RUNQ_OS=linux \
RUNQ_ARCH=amd64 \
RUNQ_INSTALL_DIR="$linux_bin" \
RUNQ_WITH_UI=0 \
RUNQ_DATA_DIR="$side_effect_dir" \
  sh "${repo_root}/install.sh" > "${tmp_dir}/default-install.log"

[ "$("${linux_bin}/runq")" = "core-linux" ]
[ ! -e "${linux_bin}/runqd" ]
[ ! -e "$side_effect_dir" ]
if grep -E 'Starting|Restarting' "${tmp_dir}/default-install.log" >/dev/null 2>&1; then
  printf 'default install unexpectedly started or restarted a daemon\n' >&2
  exit 1
fi

# Explicit versions may omit the v prefix, and the UI selection installs the
# dashboard build under the same runq command name.
darwin_bin="${tmp_dir}/darwin-bin"
RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
RUNQ_VERSION=test \
RUNQ_OS=darwin \
RUNQ_ARCH=arm64 \
RUNQ_INSTALL_DIR="$darwin_bin" \
RUNQ_WITH_UI=1 \
  sh "${repo_root}/install.sh"

[ "$("${darwin_bin}/runq")" = "ui-darwin" ]
[ ! -e "${darwin_bin}/runqd" ]

# A failed checksum must leave an existing installation untouched.
bad_release_dir="${tmp_dir}/releases/vbad"
mkdir -p "$bad_release_dir"
cp "${release_dir}/runq-linux-amd64" "${bad_release_dir}/runq-linux-amd64"
printf '%s  %s\n' '0000000000000000000000000000000000000000000000000000000000000000' 'runq-linux-amd64' \
  > "${bad_release_dir}/runq-linux-amd64.sha256"
bad_bin="${tmp_dir}/bad-bin"
mkdir -p "$bad_bin"
printf '%s\n' 'old-client' > "${bad_bin}/runq"
if RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
  RUNQ_VERSION=vbad \
  RUNQ_OS=linux \
  RUNQ_ARCH=amd64 \
  RUNQ_INSTALL_DIR="$bad_bin" \
  RUNQ_WITH_UI=0 \
    sh "${repo_root}/install.sh" >/dev/null 2>&1; then
  printf 'expected checksum mismatch to fail\n' >&2
  exit 1
fi
[ "$(cat "${bad_bin}/runq")" = "old-client" ]

if RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
  RUNQ_VERSION=vtest \
  RUNQ_OS=darwin \
  RUNQ_ARCH=arm64 \
  RUNQ_INSTALL_DIR="${tmp_dir}/invalid-bin" \
  RUNQ_WITH_UI=maybe \
    sh "${repo_root}/install.sh" >/dev/null 2>&1; then
  printf 'expected invalid RUNQ_WITH_UI to fail\n' >&2
  exit 1
fi

printf 'installer smoke tests passed\n'
