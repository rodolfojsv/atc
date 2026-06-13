#!/usr/bin/env bash
#
# build-apk-inside.sh — runs INSIDE the atc-android-builder container.
# Invoked by scripts/build-apk.sh; not meant to be run directly on the host.
#
# atc's WebView content is the small bootstrap in mobile/www/ (which then
# navigates to the live server), so unlike the sibling linuxservermanager
# build there is no separate web bundle to compile here.

set -euo pipefail

cd /workspace

log() { printf '\033[1;36m  ->\033[0m %s\n' "$*"; }

# --- Mobile / Capacitor -----------------------------------------------------

log "Installing mobile dependencies"
(cd mobile && npm install)

if [[ ! -d mobile/android ]]; then
    log "First run: adding Android platform"
    (cd mobile && npx cap add android)
fi

log "Syncing Capacitor"
(cd mobile && npx cap sync android)

# --- Inject signing config (idempotent, self-healing) -----------------------

BUILD_GRADLE="mobile/android/app/build.gradle"
SIGNING_MARKER="// ATC_SIGNING_INCLUDED"
# From mobile/android/app/build.gradle, ../.. reaches mobile/, then docker/signing.gradle.
SIGNING_APPLY="apply from: '../../docker/signing.gradle'"

log "Refreshing signing config in $BUILD_GRADLE"
sed -i "\|$SIGNING_MARKER|,+1d" "$BUILD_GRADLE"
{
    echo ""
    echo "$SIGNING_MARKER"
    echo "$SIGNING_APPLY"
} >> "$BUILD_GRADLE"

# --- Build the APK ----------------------------------------------------------

log "Assembling release APK"
(cd mobile/android && ./gradlew --no-daemon assembleRelease)

log "Done"
