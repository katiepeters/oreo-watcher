#!/bin/sh
cd `dirname $0`

# Install the TTS engine used for bark_monitor's warning/good-job voice lines,
# if it isn't already present. Prefers espeak-ng (the actively maintained
# fork); falls back to espeak. bark_monitor.go tries both binary names at
# runtime, since not every espeak-ng package provides an "espeak" alias.
if ! command -v espeak-ng >/dev/null 2>&1 && ! command -v espeak >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        echo "espeak-ng/espeak not found, attempting to install via apt-get."
        SUDO="sudo"
        if ! command -v $SUDO >/dev/null 2>&1; then
            SUDO=""
        fi
        $SUDO apt-get -qq update >/dev/null 2>&1
        if ! $SUDO apt-get install -qqy espeak-ng >/dev/null 2>&1; then
            $SUDO apt-get install -qqy espeak >/dev/null 2>&1
        fi
        if ! command -v espeak-ng >/dev/null 2>&1 && ! command -v espeak >/dev/null 2>&1; then
            echo "Warning: failed to install espeak-ng/espeak; TTS voice lines will fall back to static sounds." >&2
        fi
    else
        echo "Warning: apt-get not available; cannot install espeak-ng/espeak. TTS voice lines will fall back to static sounds." >&2
    fi
fi
