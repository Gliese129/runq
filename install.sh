#!/bin/sh

set -eu

REPOSITORY=${RUNQ_REPOSITORY:-Gliese129/runq-lab}
DOWNLOAD_BASE=${RUNQ_DOWNLOAD_BASE:-https://github.com/${REPOSITORY}/releases/download}
API_BASE=${RUNQ_API_BASE:-https://api.github.com/repos/${REPOSITORY}}

say() {
  printf '%s\n' "$*"
}

warn() {
  printf 'runq installer: warning: %s\n' "$*" >&2
}

die() {
  printf 'runq installer: error: %s\n' "$*" >&2
  exit 1
}

bool_value() {
  case "$1" in
    1|true|TRUE|yes|YES|y|Y|on|ON) printf '1\n' ;;
    0|false|FALSE|no|NO|n|N|off|OFF) printf '0\n' ;;
    *) return 1 ;;
  esac
}

command -v curl >/dev/null 2>&1 || die "curl is required"

raw_os=${RUNQ_OS:-$(uname -s)}
case "$raw_os" in
  Linux|linux) os=linux ;;
  Darwin|darwin|macOS|macos) os=darwin ;;
  *) die "unsupported operating system: ${raw_os} (Windows support is planned later)" ;;
esac

raw_arch=${RUNQ_ARCH:-$(uname -m)}
case "$raw_arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture: ${raw_arch}" ;;
esac

version=${RUNQ_VERSION:-latest}
if [ "$version" = "latest" ]; then
  release_json=$(curl -fsSL --retry 3 "${API_BASE}/releases/latest") \
    || die "could not resolve the latest release; set RUNQ_VERSION explicitly"
  version=$(printf '%s\n' "$release_json" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | sed -n '1p')
  [ -n "$version" ] \
    || die "latest release response did not include a tag; set RUNQ_VERSION explicitly"
else
  case "$version" in
    v*) ;;
    *) version="v${version}" ;;
  esac
fi
case "$version" in
  ''|*[!A-Za-z0-9._+-]*) die "invalid release version: ${version}" ;;
esac

if [ "${RUNQ_WITH_UI+x}" = "x" ]; then
  with_ui=$(bool_value "$RUNQ_WITH_UI") \
    || die "RUNQ_WITH_UI must be yes/no, true/false, or 1/0"
elif (exec </dev/tty) 2>/dev/null; then
  printf 'Install runq with the embedded web UI? [y/N] ' > /dev/tty
  answer=
  IFS= read -r answer < /dev/tty || true
  case "$answer" in
    y|Y|yes|YES) with_ui=1 ;;
    *) with_ui=0 ;;
  esac
else
  with_ui=0
  warn "no interactive terminal; installing the core CLI (set RUNQ_WITH_UI=1 to select the UI build)"
fi

if [ "${RUNQ_START_DAEMON+x}" = "x" ]; then
  start_daemon=$(bool_value "$RUNQ_START_DAEMON") \
    || die "RUNQ_START_DAEMON must be yes/no, true/false, or 1/0"
else
  start_daemon=0
fi

if [ "$with_ui" = "1" ]; then
  runq_asset="runq-dashboard-${os}-${arch}"
else
  runq_asset="runq-${os}-${arch}"
fi

if [ "${RUNQ_INSTALL_DIR+x}" = "x" ]; then
  install_dir=$RUNQ_INSTALL_DIR
elif [ "$os" = "darwin" ] && [ -d /opt/homebrew/bin ] && [ -w /opt/homebrew/bin ]; then
  install_dir=/opt/homebrew/bin
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
  install_dir=/usr/local/bin
else
  install_dir=${HOME}/.local/bin
fi

[ -n "$install_dir" ] || die "install directory cannot be empty"
mkdir -p "$install_dir" || die "could not create ${install_dir}"
[ -w "$install_dir" ] || die "${install_dir} is not writable; set RUNQ_INSTALL_DIR to a writable directory"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/runq-install.XXXXXX") \
  || die "could not create a temporary directory"
staged_path=
cleanup() {
  if [ -n "$staged_path" ]; then
    rm -f "$staged_path"
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

download_asset() {
  asset=$1
  url="${DOWNLOAD_BASE}/${version}/${asset}"
  say "Downloading ${asset} (${version})..."
  curl -fL --retry 3 --progress-bar -o "${tmp_dir}/${asset}" "$url" \
    || die "could not download ${url}"
  curl -fsSL --retry 3 -o "${tmp_dir}/${asset}.sha256" "${url}.sha256" \
    || die "could not download checksum for ${asset}"
}

verify_asset() {
  asset=$1
  expected=$(sed -n '1{s/[[:space:]].*$//;p;}' "${tmp_dir}/${asset}.sha256")
  case "$expected" in
    *[!0-9A-Fa-f]*|'') die "invalid checksum file for ${asset}" ;;
  esac
  [ "${#expected}" -eq 64 ] || die "invalid checksum file for ${asset}"

  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${tmp_dir}/${asset}" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "${tmp_dir}/${asset}" | awk '{print $1}')
  else
    die "sha256sum or shasum is required to verify downloads"
  fi
  [ "$actual" = "$expected" ] || die "checksum verification failed for ${asset}"
}

install_binary_atomic() {
  source=$1
  target=$2
  staged_path=$(mktemp "${install_dir}/.runq.new.XXXXXX") \
    || die "could not create a staging file in ${install_dir}"
  if command -v install >/dev/null 2>&1; then
    install -m 0755 "$source" "$staged_path" \
      || die "could not stage runq in ${install_dir}"
  else
    cp "$source" "$staged_path" \
      || die "could not stage runq in ${install_dir}"
    chmod 0755 "$staged_path" \
      || die "could not make the staged runq executable"
  fi
  mv -f "$staged_path" "$target" \
    || die "could not replace ${target}"
  staged_path=
}

download_asset "$runq_asset"
verify_asset "$runq_asset"

install_binary_atomic "${tmp_dir}/${runq_asset}" "${install_dir}/runq" \
  || die "could not install runq to ${install_dir}"

say "Installed runq ${version} to ${install_dir}/runq"

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *)
    warn "${install_dir} is not on PATH"
    warn "add this line to your shell profile: export PATH=\"${install_dir}:\$PATH\""
    ;;
esac

if [ "$start_daemon" = "1" ]; then
  if [ "${RUNQ_DATA_DIR+x}" = "x" ]; then
    data_dir=$RUNQ_DATA_DIR
  elif [ "$(id -u)" -eq 0 ]; then
    data_dir=/var/lib/runq
  else
    data_dir=${HOME}/.local/share/runq
  fi
  mkdir -p "$data_dir" \
    || die "runq was installed, but the data directory ${data_dir} could not be created"
  if "${install_dir}/runq" status --json >/dev/null 2>&1; then
    say "Restarting the runq client daemon to activate the installed build..."
    "${install_dir}/runq" daemon restart \
      || die "runq was installed, but the existing client daemon could not be restarted"
  else
    say "Starting runq daemon in the background..."
    "${install_dir}/runq" daemon start -d \
      || die "runq was installed, but the daemon could not be started"
  fi

  ready=0
  attempts=0
  while [ "$attempts" -lt 15 ]; do
    if "${install_dir}/runq" status --json >/dev/null 2>&1; then
      ready=1
      break
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  if [ "$ready" = "1" ]; then
    say "runq daemon is ready"
  else
    warn "the daemon was started but did not become ready within 15 seconds"
    warn "check ${data_dir}/daemon.log and run: runq doctor"
  fi
fi

if [ "$with_ui" = "1" ]; then
  say "Dashboard: http://127.0.0.1:8077"
fi
say "Next: runq doctor"
