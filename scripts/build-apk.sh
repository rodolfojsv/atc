#!/usr/bin/env bash
#
# build-apk.sh — build a signed atc Android APK using Docker.
#
# What this does:
#   1. Builds (once, cached) a Docker image with Node 22 + JDK 21 + Android SDK.
#   2. Generates a signing keystore on first run if one is not present.
#   3. Runs the full build inside the container: Capacitor sync → gradle
#      assembleRelease.
#   4. Copies the resulting signed APK to a configurable output path.
#
# The APK is a thin shell that loads your atc web UI over the tailnet; there
# is no embedded web bundle to compile, so this is faster than the sibling
# linuxservermanager build.
#
# Configuration (override via environment variables):
#   ATC_BUILDER_IMAGE     Docker image tag        (default: atc-android-builder:latest)
#   ATC_KEYSTORE_PATH     Host path to keystore   (default: $HOME/.android/atc-release.keystore)
#   ATC_KEYSTORE_PASSWORD Keystore password       (REQUIRED — no default)
#   ATC_KEY_PASSWORD      Key password            (default: same as keystore password)
#   ATC_KEY_ALIAS         Key alias               (default: atc)
#   ATC_OUTPUT_APK        Where to copy the APK   (default: ./build/atc.apk)
#
# Requirements on the host:
#   - docker (daemon accessible to the running user)
#   - keytool (first run only, to generate the keystore — ships with any JDK)
#   - bash, coreutils

set -euo pipefail

IMAGE_TAG="${ATC_BUILDER_IMAGE:-atc-android-builder:latest}"
KEYSTORE_PATH="${ATC_KEYSTORE_PATH:-$HOME/.android/atc-release.keystore}"
KEYSTORE_PASSWORD="${ATC_KEYSTORE_PASSWORD:-}"
KEY_PASSWORD="${ATC_KEY_PASSWORD:-$KEYSTORE_PASSWORD}"
KEY_ALIAS="${ATC_KEY_ALIAS:-atc}"
OUTPUT_APK="${ATC_OUTPUT_APK:-./build/atc.apk}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
err() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; }

# --- Sanity checks ----------------------------------------------------------

if ! command -v docker >/dev/null 2>&1; then
    err "docker is not installed or not on PATH."
    exit 1
fi

if [[ -z "$KEYSTORE_PASSWORD" ]]; then
    err "ATC_KEYSTORE_PASSWORD is required."
    err "Set it (and optionally ATC_KEY_PASSWORD) and rerun, e.g.:"
    err "  ATC_KEYSTORE_PASSWORD=hunter2 ./scripts/build-apk.sh"
    exit 1
fi

# --- Builder image ----------------------------------------------------------

if ! docker image inspect "$IMAGE_TAG" >/dev/null 2>&1; then
    log "Building Docker image $IMAGE_TAG (one-time, expect ~5-10 min and ~2 GB)"
    docker build -t "$IMAGE_TAG" -f mobile/docker/Dockerfile mobile/docker
else
    log "Builder image $IMAGE_TAG present (skipping image build)"
fi

# --- Keystore ---------------------------------------------------------------

if [[ ! -f "$KEYSTORE_PATH" ]]; then
    log "No keystore at $KEYSTORE_PATH — generating a new self-signed one"
    if ! command -v keytool >/dev/null 2>&1; then
        err "keytool not found on host. Install a JDK (e.g. apt install default-jdk-headless)"
        err "or pre-generate the keystore by hand and rerun."
        exit 1
    fi
    mkdir -p "$(dirname "$KEYSTORE_PATH")"
    keytool -genkeypair -v \
        -keystore "$KEYSTORE_PATH" \
        -alias "$KEY_ALIAS" \
        -keyalg RSA -keysize 2048 -validity 10000 \
        -storepass "$KEYSTORE_PASSWORD" \
        -keypass "$KEY_PASSWORD" \
        -dname "CN=atc, OU=Self, O=Self, L=Self, S=Self, C=US"
    log "Keystore created at $KEYSTORE_PATH — back this file up. You cannot replace"
    log "it later without uninstalling existing installs."
fi

# --- Run the build inside the container -------------------------------------

log "Running build inside $IMAGE_TAG"

HOST_UID="$(id -u)"
HOST_GID="$(id -g)"
GRADLE_CACHE="${ATC_GRADLE_CACHE:-$HOME/.gradle-atc-builder}"
mkdir -p "$GRADLE_CACHE"

# The container runs as the host UID so generated files aren't root-owned,
# but that UID has no entry in the image's /etc/passwd — and several Node
# tools (Capacitor CLI) call os.userInfo() and crash with ENOENT. Inject a
# minimal passwd/group with just the caller.
PASSWD_FILE="$(mktemp)"
GROUP_FILE="$(mktemp)"
trap 'rm -f "$PASSWD_FILE" "$GROUP_FILE"' EXIT
printf 'builder:x:%s:%s:builder:/home/builder:/bin/bash\n' "$HOST_UID" "$HOST_GID" > "$PASSWD_FILE"
printf 'builder:x:%s:\n' "$HOST_GID" > "$GROUP_FILE"

docker run --rm \
    -u "$HOST_UID:$HOST_GID" \
    -v "$REPO_ROOT":/workspace \
    -v "$KEYSTORE_PATH":/keystore/release.keystore:ro \
    -v "$GRADLE_CACHE":/home/builder/.gradle \
    -v "$PASSWD_FILE":/etc/passwd:ro \
    -v "$GROUP_FILE":/etc/group:ro \
    -e HOME=/home/builder \
    -e USER=builder \
    -e ATC_KEYSTORE_FILE=/keystore/release.keystore \
    -e ATC_KEYSTORE_PASSWORD="$KEYSTORE_PASSWORD" \
    -e ATC_KEY_PASSWORD="$KEY_PASSWORD" \
    -e ATC_KEY_ALIAS="$KEY_ALIAS" \
    -w /workspace \
    "$IMAGE_TAG" \
    bash scripts/build-apk-inside.sh

# --- Copy the APK out -------------------------------------------------------

APK_SRC="mobile/android/app/build/outputs/apk/release/app-release.apk"
if [[ ! -f "$APK_SRC" ]]; then
    err "Build finished but no APK at $APK_SRC"
    exit 1
fi

mkdir -p "$(dirname "$OUTPUT_APK")"
cp "$APK_SRC" "$OUTPUT_APK"

SHA="$(sha256sum "$OUTPUT_APK" | awk '{print $1}')"
SIZE="$(stat -c %s "$OUTPUT_APK")"

log "APK ready"
printf '    path:   %s\n' "$OUTPUT_APK"
printf '    size:   %s bytes\n' "$SIZE"
printf '    sha256: %s\n' "$SHA"
echo
echo "Point web.apkPath at this file and set web.apkVersion in config.json,"
echo "then restart atc — the web 'App' tab will serve it with a download QR."
