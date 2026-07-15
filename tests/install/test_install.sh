#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/runq-installer-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

release_dir="${tmp_dir}/releases/vtest"
mkdir -p "$release_dir"

make_asset() {
  name=$1
  marker=$2
  cat > "${release_dir}/${name}" <<EOF
#!/bin/sh
printf '%s\\n' '${marker}'
EOF
  chmod +x "${release_dir}/${name}"
  (
    cd "$release_dir"
    sha256sum "$name" > "${name}.sha256"
  )
}

make_asset runq-linux-amd64 core-linux
make_asset runq-dashboard-linux-amd64 ui-linux
make_asset runqd-linux-amd64 runqd-linux
make_asset runq-darwin-arm64 core-darwin
make_asset runq-dashboard-darwin-arm64 ui-darwin

linux_bin="${tmp_dir}/linux-bin"
RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
RUNQ_VERSION=vtest \
RUNQ_OS=linux \
RUNQ_ARCH=amd64 \
RUNQ_INSTALL_DIR="$linux_bin" \
RUNQ_WITH_UI=0 \
RUNQ_START_DAEMON=0 \
  sh "${repo_root}/install.sh"

[ "$("${linux_bin}/runq")" = "core-linux" ]
[ "$("${linux_bin}/runqd")" = "runqd-linux" ]

darwin_bin="${tmp_dir}/darwin-bin"
RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
RUNQ_VERSION=vtest \
RUNQ_OS=darwin \
RUNQ_ARCH=arm64 \
RUNQ_INSTALL_DIR="$darwin_bin" \
RUNQ_WITH_UI=1 \
RUNQ_START_DAEMON=0 \
  sh "${repo_root}/install.sh"

[ "$("${darwin_bin}/runq")" = "ui-darwin" ]
[ ! -e "${darwin_bin}/runqd" ]

if RUNQ_DOWNLOAD_BASE="file://${tmp_dir}/releases" \
  RUNQ_VERSION=vtest \
  RUNQ_OS=darwin \
  RUNQ_ARCH=arm64 \
  RUNQ_INSTALL_DIR="${tmp_dir}/invalid-bin" \
  RUNQ_WITH_UI=maybe \
  RUNQ_START_DAEMON=0 \
    sh "${repo_root}/install.sh" >/dev/null 2>&1; then
  printf 'expected invalid RUNQ_WITH_UI to fail\n' >&2
  exit 1
fi

printf 'installer smoke tests passed\n'
