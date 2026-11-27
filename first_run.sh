#!/bin/sh
# Runs once on the machine, before the module first starts (meta.json
# "first_run"). Installs what bark-monitor needs to speak: the Piper engine and
# its voice, plus espeak-ng as the fallback. Always exits 0: a machine that
# can't install them (no network, unsupported CPU) should still run bark
# detection, just without the spoken lines.

PIPER_DIR="/opt/pupster/piper"
PIPER_VERSION="2023.11.14-2"
PIPER_VOICE="en_US-kristin-medium"
PIPER_SHA256_aarch64="fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb"
PIPER_SHA256_x86_64="a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992"
VOICE_SHA256="5849957f929cbf720c258f8458692d6103fff2f0e3d3b19c8259474bb06a18d4"
VOICE_CONFIG_SHA256="5681426d4aead22195de70531eeeeddb46493cfaffc5764b2ea3db73428b651c"
VOICE_URL="https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/kristin/medium/$PIPER_VOICE.onnx"

SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
fi

warn() {
    echo "WARNING: $*" >&2
}

apt_install() {
    if ! command -v apt-get >/dev/null 2>&1; then
        return 1
    fi
    if [ -z "$APT_UPDATED" ]; then
        DEBIAN_FRONTEND=noninteractive $SUDO apt-get -qq update
        APT_UPDATED=1
    fi
    DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -qqy "$@"
}

# fetch URL DEST SHA256: download to DEST, failing unless the checksum matches.
fetch() {
    curl -fsSL --retry 3 -o "$2" "$1" || return 1
    echo "$3  $2" | sha256sum -c - >/dev/null 2>&1
}

install_espeak() {
    if command -v espeak-ng >/dev/null 2>&1 || command -v espeak >/dev/null 2>&1; then
        echo "espeak already installed."
        return 0
    fi
    echo "Installing espeak-ng."
    apt_install espeak-ng || apt_install espeak ||
        warn "could not install espeak-ng; run 'sudo apt-get install -y espeak-ng' on this machine."
}

install_piper() {
    if [ -x "$PIPER_DIR/piper/piper" ] && [ -f "$PIPER_DIR/$PIPER_VOICE.onnx" ] && [ -f "$PIPER_DIR/$PIPER_VOICE.onnx.json" ]; then
        echo "Piper and the $PIPER_VOICE voice already installed."
        return 0
    fi
    case "$(uname -s)-$(uname -m)" in
        Linux-aarch64 | Linux-arm64) arch="aarch64" sum="$PIPER_SHA256_aarch64" ;;
        Linux-x86_64) arch="x86_64" sum="$PIPER_SHA256_x86_64" ;;
        *)
            warn "no Piper build for $(uname -s) $(uname -m); spoken lines will use espeak."
            return 0
            ;;
    esac
    if ! command -v curl >/dev/null 2>&1; then
        apt_install curl || { warn "curl is unavailable; cannot download Piper."; return 0; }
    fi

    tmp="$(mktemp -d)" || return 0
    echo "Installing Piper $PIPER_VERSION and the $PIPER_VOICE voice into $PIPER_DIR."
    if fetch "https://github.com/rhasspy/piper/releases/download/$PIPER_VERSION/piper_linux_$arch.tar.gz" "$tmp/piper.tar.gz" "$sum" &&
        fetch "$VOICE_URL" "$tmp/$PIPER_VOICE.onnx" "$VOICE_SHA256" &&
        fetch "$VOICE_URL.json" "$tmp/$PIPER_VOICE.onnx.json" "$VOICE_CONFIG_SHA256" &&
        $SUDO mkdir -p "$PIPER_DIR" &&
        $SUDO tar -xzf "$tmp/piper.tar.gz" -C "$PIPER_DIR" &&
        $SUDO mv "$tmp/$PIPER_VOICE.onnx" "$tmp/$PIPER_VOICE.onnx.json" "$PIPER_DIR/"; then
        echo "Piper installed."
    else
        warn "could not download or verify Piper; spoken lines will use espeak."
    fi
    rm -rf "$tmp"
}

install_espeak
install_piper
exit 0
