#!/usr/bin/env bash
set +x
set -Eeuo pipefail
umask 077

PROGRAM=snell-agent
CONFIG=/etc/snell-agent/config.json
STATE=/var/lib/snell-agent
BINARY=/usr/local/bin/snell-agent
UNIT=/etc/systemd/system/snell-agent.service
SCRIPT_DIR=
script_path=${BASH_SOURCE[0]:-}
if [[ -n $script_path && -f $script_path && $script_path != /dev/fd/* && $script_path != /proc/* ]]; then
    SCRIPT_DIR=$(cd -- "$(dirname -- "$script_path")" && pwd)
fi
source_binary= binary_url= checksum= config_source= panel_url= node_id= token_file= token=
token_supplied=0
release_repo=${SNELL_AGENT_RELEASE_REPO:-Micah123321/mi-node}
release_version=${SNELL_AGENT_VERSION:-latest}
version_selected=0
build=0
start=1
stage=
changed=0
was_active=0
enabled_state=disabled
backup=
new_config=0

log() { printf '%s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'USAGE'
Install or upgrade the standalone Snell Agent on Linux with systemd.

  bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/snell-agent/install.sh) \
    -a https://panel.example.com -t TOKEN -n 41
  sudo bash install.sh
  sudo bash install.sh --config /root/snell-config.json --binary ./snell-agent-linux-amd64

Options:
  -a, --api, --panel-url URL  Panel root URL (HTTP or HTTPS).
  -t, --token TOKEN          Panel server token; never printed in installer logs.
  -n, --node-id ID           Positive numeric panel node ID.
  --token-file FILE          Read a token from a single-line file instead of -t.
  --version TAG              Install a fixed snell-v tag; default: latest complete Snell release.
  --release-repo OWNER/REPO  Release repository; default: Micah123321/mi-node.
  --binary FILE              Install a local Linux binary instead of downloading.
  --binary-url URL           Download a custom HTTPS binary; requires --sha256.
  --sha256 HEX               Check SHA256 (automatic for standard GitHub release downloads).
  --build                    Build the local independent source module; requires Go.
  --config FILE              Import a complete configuration on first installation.
  --no-start, --skip-start   Install and enable the service, leaving it stopped.
  -h, --help                 Show this help.

No local files or Go installation are needed for curl-based installation.
Snell uses its own encryption; --cert-* options do not apply.
Existing configuration and /var/lib/snell-agent are preserved on upgrade.
Repeating matching -a/-t/-n parameters keeps the existing configuration unchanged.
Without panel parameters, first-install prompts use /dev/tty and hide the token.
USAGE
}
need_value() { [[ $# -ge 2 && -n $2 && $2 != --* ]] || die "Missing value for $1"; }
while [[ $# -gt 0 ]]; do
    case "$1" in
        --binary) need_value "$@"; source_binary=$2; shift 2 ;;
        --binary-url) need_value "$@"; binary_url=$2; shift 2 ;;
        --sha256) need_value "$@"; checksum=$2; shift 2 ;;
        --config) need_value "$@"; config_source=$2; shift 2 ;;
        -a|--api|--panel|--panel-url) need_value "$@"; panel_url=$2; shift 2 ;;
        -t|--token) need_value "$@"; token=$2; token_supplied=1; shift 2 ;;
        -n|--node-id) need_value "$@"; node_id=$2; shift 2 ;;
        --token-file) need_value "$@"; token_file=$2; shift 2 ;;
        --version) need_value "$@"; release_version=$2; version_selected=1; shift 2 ;;
        --release-repo) need_value "$@"; release_repo=$2; shift 2 ;;
        --cert-*) die "Snell v4 does not use TLS certificates; remove --cert-* options." ;;
        --build) build=1; shift ;;
        --no-start|--skip-start) start=0; shift ;;
        install|upgrade|update) shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "Unknown option: $1" ;;
    esac
done
[[ $(uname -s) == Linux ]] || die "This installer supports Linux only."
[[ $EUID -eq 0 ]] || die "Run this script with sudo or as root."
[[ -d /run/systemd/system ]] || die "A running systemd host is required."
if ! command -v python3 >/dev/null || ! command -v curl >/dev/null || ! command -v flock >/dev/null; then
    log "Installing Python 3, curl, and util-linux dependencies."
    if command -v apt-get >/dev/null; then
        DEBIAN_FRONTEND=noninteractive apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y python3 curl ca-certificates util-linux passwd
    elif command -v dnf >/dev/null; then
        dnf install -y python3 curl ca-certificates util-linux shadow-utils
    elif command -v yum >/dev/null; then
        yum install -y python3 curl ca-certificates util-linux shadow-utils
    else
        die "Install python3, curl, and flock before running this installer."
    fi
fi
for command in python3 systemctl install mktemp cp mv rm chmod chown getent groupadd useradd id flock cat date sleep sha256sum; do
    command -v "$command" >/dev/null || die "Missing dependency: $command"
done
case "$(uname -m)" in
    x86_64|amd64) arch=amd64; elf_machine=62 ;;
    aarch64|arm64) arch=arm64; elf_machine=183 ;;
    *) die "Supported architectures: amd64 and arm64." ;;
esac
sources=$build
[[ -z $source_binary ]] || sources=$((sources + 1))
[[ -z $binary_url ]] || sources=$((sources + 1))
[[ $sources -le 1 ]] || die "Select only one of --binary, --binary-url, or --build."
[[ -z $checksum || $checksum =~ ^[[:xdigit:]]{64}$ ]] || die "SHA256 must contain exactly 64 hexadecimal characters."
if [[ -n $binary_url ]]; then
    [[ $binary_url == https://* && -n $checksum ]] || die "Downloads require an HTTPS URL and --sha256."
    command -v curl >/dev/null || die "Missing dependency: curl"
fi
if [[ -n $config_source && ( -n $panel_url || -n $node_id || -n $token_file || $token_supplied -eq 1 ) ]]; then
    die "Use --config or the individual panel parameters, not both."
fi
[[ -z $token_file || $token_supplied -eq 0 ]] || die "Use --token or --token-file, not both."
[[ $release_repo =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "Release repository must be OWNER/REPO."
[[ $release_version == latest || $release_version =~ ^snell-v[A-Za-z0-9._-]+$ ]] || die "Version must be latest or a snell-v release tag."
[[ $version_selected -eq 0 || $sources -eq 0 ]] || die "Use --version with automatic downloads, not a local or custom binary source."
for path in /usr/local/bin /etc/snell-agent /var/lib/snell-agent "$BINARY" "$CONFIG" "$UNIT"; do
    [[ ! -L $path ]] || die "Installation target must not be a symlink: $path"
done
lock_dir=/run/snell-agent-installer
[[ ! -L $lock_dir ]] || die "Installer lock directory must not be a symlink."
install -d -m 0700 -o root -g root "$lock_dir"
[[ ! -L $lock_dir/install.lock && ( ! -e $lock_dir/install.lock || -f $lock_dir/install.lock ) ]] || die "Invalid installer lock file."
exec 9>>"$lock_dir/install.lock"
flock -n 9 || die "Another Snell Agent installation is running."
stage=$(mktemp -d /tmp/snell-agent-install.XXXXXXXX)

# Restore the prior installation if replacing files or starting the service fails.
restore_file() {
    local key=$1 target=$2
    if [[ -f $backup/$key ]]; then
        cp -p -- "$backup/$key" "$target.rollback-$$"
        mv -fT -- "$target.rollback-$$" "$target"
    else
        rm -f -- "$target"
    fi
}
cleanup() {
    local status=$?
    trap - EXIT
    set +e
    if [[ $status -ne 0 && $changed -eq 1 ]]; then
        log "Installation failed; restoring the previous program, configuration, and service."
        systemctl stop "$PROGRAM.service" >/dev/null 2>&1
        restore_file binary "$BINARY"
        restore_file config "$CONFIG"
        restore_file service "$UNIT"
        systemctl daemon-reload
        systemctl disable "$PROGRAM.service" >/dev/null 2>&1
        systemctl disable --runtime "$PROGRAM.service" >/dev/null 2>&1
        case "$enabled_state" in
            enabled) systemctl enable "$PROGRAM.service" >/dev/null 2>&1 ;;
            enabled-runtime) systemctl enable --runtime "$PROGRAM.service" >/dev/null 2>&1 ;;
        esac
        if [[ $was_active -eq 1 ]]; then
            systemctl reset-failed "$PROGRAM.service" >/dev/null 2>&1 || true
            systemctl start "$PROGRAM.service" || log "Previous service could not restart; inspect journalctl -u snell-agent."
        fi
        log "Report state was retained. Backup: $backup"
    fi
    rm -f -- "$BINARY.install-$$" "$CONFIG.install-$$" "$UNIT.install-$$"
    [[ -z $stage ]] || rm -rf -- "$stage"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

download() {
    command -v curl >/dev/null || die "Missing dependency: curl"
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 300 --retry 2 --output "$2" "$1"
}
release_binary() {
    local asset="snell-agent-linux-$arch" tag=$release_version page
    if [[ $tag == latest ]]; then
        tag=
        for page in 1 2 3 4 5; do
            download "https://api.github.com/repos/$release_repo/releases?per_page=100&page=$page" "$stage/releases.json"
            tag=$(python3 -c '
import json, re, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        releases = json.load(f)
    if not isinstance(releases, list):
        raise ValueError()
    candidates = []
    for release in releases:
        name = release.get("tag_name", "")
        assets = {a.get("name") for a in release.get("assets", [])}
        if not release.get("draft") and re.fullmatch(r"snell-v[A-Za-z0-9._-]+", name) and {sys.argv[2], "SHA256SUMS"} <= assets:
            candidates.append(release)
    if candidates:
        print(max(candidates, key=lambda r: r.get("published_at") or r.get("created_at") or "")["tag_name"])
    elif not releases:
        print("__empty__")
except Exception:
    sys.exit("Error: invalid GitHub release response.")
' "$stage/releases.json" "$asset")
            [[ $tag != __empty__ ]] || die "No complete Snell Agent release found in $release_repo."
            [[ -z $tag ]] || break
        done
        [[ -n $tag ]] || die "No complete Snell Agent release found in the first 500 releases."
    fi
    log "Downloading Snell Agent $tag ($arch)."
    local base="https://github.com/$release_repo/releases/download/$tag"
    download "$base/SHA256SUMS" "$stage/SHA256SUMS"
    local release_checksum
    release_checksum=$(python3 -c '
import re, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        rows = [line.split() for line in f]
    hashes = [row[0] for row in rows if len(row) == 2 and row[1].lstrip("*") == sys.argv[2]]
    if len(hashes) != 1 or not re.fullmatch(r"[A-Fa-f0-9]{64}", hashes[0]):
        raise ValueError()
    print(hashes[0])
except Exception:
    sys.exit("Error: release SHA256SUMS must contain exactly one valid binary checksum.")
' "$stage/SHA256SUMS" "$asset")
    [[ -z $checksum || ${checksum,,} == ${release_checksum,,} ]] || die "Requested checksum does not match the selected release."
    checksum=$release_checksum
    download "$base/$asset" "$stage/snell-agent"
}

if [[ -n $binary_url ]]; then
    log "Downloading the selected Agent binary."
    download "$binary_url" "$stage/snell-agent"
elif [[ $build -eq 1 ]]; then
    [[ -n $SCRIPT_DIR && -f $SCRIPT_DIR/go.mod && -d $SCRIPT_DIR/cmd/snell-agent ]] || die "Source module is missing beside install.sh."
    command -v go >/dev/null || die "Building requires Go 1.24.7 or newer."
    log "Building the independent Snell Agent module."
    (cd -- "$SCRIPT_DIR" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
        go build -trimpath -o "$stage/snell-agent" ./cmd/snell-agent)
else
    if [[ -z $source_binary && -n $SCRIPT_DIR && $version_selected -eq 0 ]]; then
        for candidate in "$SCRIPT_DIR/bin/snell-agent-linux-$arch" "$SCRIPT_DIR/snell-agent-linux-$arch"; do
            if [[ -f $candidate ]]; then source_binary=$candidate; break; fi
        done
    fi
    if [[ -n $source_binary ]]; then
        [[ -f $source_binary ]] || die "Local binary source is not a regular file."
        cp -- "$source_binary" "$stage/snell-agent"
    else
        release_binary
    fi
fi
if [[ -n $checksum ]]; then
    command -v sha256sum >/dev/null || die "Missing dependency: sha256sum"
    actual=$(sha256sum "$stage/snell-agent")
    actual=${actual%% *}
    [[ ${actual,,} == ${checksum,,} ]] || die "Binary SHA256 mismatch; installation was not changed."
fi
python3 -c '
import struct, sys
try:
    with open(sys.argv[1], "rb") as f:
        h = f.read(20)
    if len(h) != 20 or h[:6] != b"\x7fELF\x02\x01" or struct.unpack("<H", h[18:20])[0] != int(sys.argv[2]):
        raise ValueError()
except Exception:
    sys.exit("Error: binary is not a Linux 64-bit ELF for this architecture.")
' "$stage/snell-agent" "$elf_machine"
chmod 0755 "$stage/snell-agent"

if [[ -n $token_file ]]; then
    [[ -f $token_file && -r $token_file ]] || die "Token file is not readable."
    token=$(<"$token_file")
    token_supplied=1
fi
if [[ $token_supplied -eq 1 ]]; then
    [[ -n $token && $token != *$'\n'* && $token != *$'\r'* ]] || die "Token must be a nonempty single line."
fi
if [[ -e $CONFIG ]]; then
    [[ -f $CONFIG ]] || die "Existing configuration is not a regular file."
    [[ -z $config_source ]] || die "Configuration already exists and is preserved; edit /etc/snell-agent/config.json separately."
    if [[ -n $panel_url || -n $node_id || $token_supplied -eq 1 ]]; then
        printf '%s\0' "$panel_url" "$node_id" "$token" | python3 -c '
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        cfg = json.load(f)
    panel, node, token, end = sys.stdin.buffer.read().decode("utf-8").split("\0")
    if (panel and panel != cfg.get("panel_url")) or (node and int(node) != cfg.get("node_id")) or (sys.argv[2] == "1" and token != cfg.get("token")):
        raise ValueError()
except Exception:
    sys.exit("Error: Configuration already exists; supplied panel parameters differ or are invalid. Existing configuration was preserved.")
' "$CONFIG" "$token_supplied"
    fi
    unset token
    cp -- "$CONFIG" "$stage/config.json"
    log "Keeping the existing configuration and report state."
elif [[ -n $config_source ]]; then
    [[ -f $config_source ]] || die "Configuration source is not a regular file."
    cp -- "$config_source" "$stage/config.json"
    new_config=1
else
    prompt() {
        [[ -r /dev/tty ]] || die "No interactive terminal; provide --panel-url, --node-id and --token-file, or --config."
        IFS= read -r -p "$1" "$2" </dev/tty || die "Input was interrupted."
    }
    [[ -n $panel_url ]] || prompt 'Panel root URL (https://...): ' panel_url
    [[ -n $node_id ]] || prompt 'Snell node numeric ID: ' node_id
    if [[ $token_supplied -eq 0 ]]; then
        [[ -r /dev/tty ]] || die "No interactive terminal; provide --token or --token-file."
        IFS= read -r -s -p 'Panel server token (hidden): ' token </dev/tty || die "Input was interrupted."
        printf '\n' >/dev/tty
    fi
    [[ -n $token && $token != *$'\n'* && $token != *$'\r'* ]] || die "Token must be a nonempty single line."
    [[ $node_id =~ ^[1-9][0-9]*$ ]] || die "Node ID must be a positive integer."
    # A NUL-framed stdin keeps the token out of child process arguments and JSON-escapes it.
    printf '%s\0' "$panel_url" "$node_id" "$token" | python3 -c '
import json, sys
try:
    panel, node, token, end = sys.stdin.buffer.read().decode("utf-8").split("\0")
    config = {"panel_url": panel, "node_id": int(node), "token": token,
              "state_dir": "/var/lib/snell-agent", "stale_after": "5m"}
except (ValueError, UnicodeError):
    sys.exit("Error: invalid configuration input.")
json.dump(config, sys.stdout, indent=2)
sys.stdout.write("\n")
' >"$stage/config.json"
    unset token
    new_config=1
fi
"$stage/snell-agent" -check-config -config "$stage/config.json"
python3 -c '
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    config = json.load(f)
if config.get("state_dir") != "/var/lib/snell-agent":
    sys.exit("Error: systemd installation requires state_dir=/var/lib/snell-agent; configuration was preserved.")
' "$stage/config.json"

cat >"$stage/snell-agent.service" <<'SERVICE'
[Unit]
Description=mi-node standalone Snell TCP v4 Agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=snell-agent
Group=snell-agent
StateDirectory=snell-agent
StateDirectoryMode=0700
WorkingDirectory=/var/lib/snell-agent
ExecStart=/usr/local/bin/snell-agent -config /etc/snell-agent/config.json
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30
LimitNOFILE=65536
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/snell-agent
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
SERVICE

if ! getent group "$PROGRAM" >/dev/null; then groupadd --system "$PROGRAM"; fi
if ! id -u "$PROGRAM" >/dev/null 2>&1; then
    useradd --system --gid "$PROGRAM" --home-dir "$STATE" --no-create-home --shell /usr/sbin/nologin "$PROGRAM"
fi
[[ $(id -u "$PROGRAM") != 0 ]] || die "The snell-agent account must not use UID 0."
install -d -m 0755 /usr/local/bin /etc/systemd/system
install -d -m 0750 -o root -g "$PROGRAM" /etc/snell-agent
install -d -m 0700 -o "$PROGRAM" -g "$PROGRAM" "$STATE"
systemctl is-active --quiet "$PROGRAM.service" && was_active=1
service_state=$(systemctl is-active "$PROGRAM.service" 2>/dev/null || true)
case "$service_state" in activating|reloading) was_active=1 ;; esac
enabled_state=$(systemctl is-enabled "$PROGRAM.service" 2>/dev/null || true)
backup=/var/backups/snell-agent/$(date -u +%Y%m%dT%H%M%SZ)-$$
install -d -m 0700 "$backup"
[[ ! -f $BINARY ]] || cp -p -- "$BINARY" "$backup/binary"
[[ ! -f $CONFIG ]] || cp -p -- "$CONFIG" "$backup/config"
[[ ! -f $UNIT ]] || cp -p -- "$UNIT" "$backup/service"
changed=1
# Stop existing units even when inactive or waiting for an automatic restart.
if [[ -f $UNIT || $was_active -eq 1 ]]; then systemctl stop "$PROGRAM.service"; fi
atomic_install() {
    local source=$1 target=$2 mode=$3 owner=$4 group=$5
    install -m "$mode" -o "$owner" -g "$group" -- "$source" "$target.install-$$"
    mv -fT -- "$target.install-$$" "$target"
}
atomic_install "$stage/snell-agent" "$BINARY" 0755 root root
if [[ $new_config -eq 1 ]]; then
    atomic_install "$stage/config.json" "$CONFIG" 0600 "$PROGRAM" "$PROGRAM"
else
    chown "$PROGRAM:$PROGRAM" "$CONFIG"
    chmod 0600 "$CONFIG"
fi
atomic_install "$stage/snell-agent.service" "$UNIT" 0644 root root
systemctl daemon-reload
systemctl enable "$PROGRAM.service"
if [[ $start -eq 1 ]]; then
    systemctl reset-failed "$PROGRAM.service" >/dev/null 2>&1 || true
    systemctl start "$PROGRAM.service"
    sleep 1
    systemctl is-active --quiet "$PROGRAM.service" || die "Service exited after startup; rolling back."
    log "Snell Agent installed; the service process is active."
else
    log "Snell Agent installed and enabled; the service is stopped (--no-start)."
fi
changed=0
log "Configuration: $CONFIG"
log "Persistent report state: $STATE"
log "Backup: $backup"
log "Check panel sync and listener status: journalctl -u snell-agent -f"
log "Allow the node TCP port in your firewall; use ports 1024 or higher."
