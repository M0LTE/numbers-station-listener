#!/usr/bin/env bash
# Starts a throwaway PulseAudio with a null sink, without root and without
# touching any running session, so headless Firefox has an audio device.
# Debs are fetched with `apt-get download` and unpacked under the cache dir.
#
#   eval "$(scripts/webm-playback/pulse-null.sh)"   # exports PULSE_SERVER
#   node scripts/webm-playback/check.mjs
#   kill "$NSL_PULSE_PID"
set -euo pipefail

cache="${XDG_CACHE_HOME:-$HOME/.cache}/nsl-pulse-null"
run="${TMPDIR:-/tmp}/nsl-pulse-$UID"
mkdir -p "$cache/debs" "$run"

if [ ! -x "$cache/root/usr/bin/pulseaudio" ]; then
	(cd "$cache/debs" && apt-get download pulseaudio libltdl7 libsoxr0 libspeexdsp1 >&2)
	for d in "$cache"/debs/*.deb; do dpkg -x "$d" "$cache/root"; done
fi

mods=$(dirname "$(find "$cache/root" -name module-null-sink.so | head -n1)")
libs="$cache/root/usr/lib/x86_64-linux-gnu:$cache/root/usr/lib/x86_64-linux-gnu/pulseaudio:$mods"

env -u DBUS_SESSION_BUS_ADDRESS LD_LIBRARY_PATH="$libs" PULSE_RUNTIME_PATH="$run" \
	HOME="$run" XDG_CONFIG_HOME="$run" \
	"$cache/root/usr/bin/pulseaudio" -n --daemonize=no --exit-idle-time=-1 \
	--disable-shm=yes --use-pid-file=no --system=no --dl-search-path="$mods" \
	-L "module-null-sink sink_name=null rate=48000" \
	-L "module-native-protocol-unix socket=$run/native auth-anonymous=1" \
	-L "module-always-sink" >"$run/log" 2>&1 &
pid=$!

for _ in $(seq 50); do
	[ -S "$run/native" ] && break
	sleep 0.1
done
if [ ! -S "$run/native" ]; then
	echo "pulseaudio did not start; see $run/log" >&2
	exit 1
fi
echo "export PULSE_SERVER=unix:$run/native NSL_PULSE_PID=$pid"
