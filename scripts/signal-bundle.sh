#!/bin/sh
# signal-bundle.sh ARCH DIR packs signal-cli's JVM build for linux/ARCH, as
# the Dockerfile's signal stage makes it, into
# DIR/signal-cli-jvm-linux-ARCH.tar.gz for say-update: opt/java (the JRE),
# opt/signal-cli/lib (jars and libsignal) and usr/local/bin/signal-cli (the
# launcher), laid out from /. Needs docker with BuildKit.
set -eu
arch=${1:?usage: signal-bundle.sh ARCH DIR}
dir=${2:?usage: signal-bundle.sh ARCH DIR}
cd "$(dirname "$0")/.."
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
docker build --target signal --platform "linux/$arch" --build-arg SIGNAL_CLI_BUILD=jvm \
  --output "type=local,dest=$out" .
tar -C "$out/out" --owner=0 --group=0 -czf "$dir/signal-cli-jvm-linux-$arch.tar.gz" \
  opt/java opt/signal-cli usr/local/bin/signal-cli
