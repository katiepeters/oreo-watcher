#!/bin/sh
# Runs once on the machine, before the module first starts (meta.json
# "first_run"). Installs the text-to-speech engine behind bark-monitor's spoken
# lines. Always exits 0: a machine that can't install it (no network, no
# apt-get) should still run bark detection, just without the computer voice.

if command -v espeak-ng >/dev/null 2>&1 || command -v espeak >/dev/null 2>&1; then
    echo "TTS engine already installed."
    exit 0
fi

if ! command -v apt-get >/dev/null 2>&1; then
    echo "WARNING: apt-get not available; install espeak-ng manually to enable spoken lines." >&2
    exit 0
fi

SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
fi

echo "Installing espeak-ng for spoken lines."
export DEBIAN_FRONTEND=noninteractive
$SUDO apt-get -qq update
$SUDO apt-get install -qqy espeak-ng || $SUDO apt-get install -qqy espeak

if ! command -v espeak-ng >/dev/null 2>&1 && ! command -v espeak >/dev/null 2>&1; then
    echo "WARNING: could not install espeak-ng; run 'sudo apt-get install -y espeak-ng' on this machine to enable spoken lines." >&2
fi
exit 0
