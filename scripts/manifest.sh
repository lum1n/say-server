#!/bin/sh
# manifest.sh VERSION IMAGE_DIGEST DIR writes DIR/SHA256SUMS and
# DIR/manifest.json for the release vVERSION (ADR 0017). DIR holds
# say-linux-amd64 and say-linux-arm64, and signal-cli-jvm-linux-amd64.tar.gz
# and signal-cli-jvm-linux-arm64.tar.gz from scripts/signal-bundle.sh. Both
# release.yml and release.sh use it, so the two make the same manifest.
set -eu
version=$1 digest=$2 dir=$3
root=$(cd "$(dirname "$0")/.." && pwd)
repo=${GITHUB_REPOSITORY:-lum1n/say-server}
base="https://github.com/$repo/releases/download/v$version"
# signal-cli comes from its own releases, pinned in the Dockerfile.
sig_version=$(sed -n 's/^ARG SIGNAL_CLI_VERSION=//p' "$root/Dockerfile")
sig_sha=$(sed -n 's/^ARG SIGNAL_CLI_SHA256=//p' "$root/Dockerfile")

cd "$dir"
sha256sum say-linux-amd64 say-linux-arm64 signal-cli-jvm-linux-amd64.tar.gz signal-cli-jvm-linux-arm64.tar.gz >SHA256SUMS
jq -n \
  --arg version "$version" \
  --arg image "ghcr.io/lum1n/say-node:$version" \
  --arg digest "$digest" \
  --arg base "$base" \
  --arg amd64 "$(sha256sum say-linux-amd64 | cut -d' ' -f1)" \
  --arg arm64 "$(sha256sum say-linux-arm64 | cut -d' ' -f1)" \
  --arg sig_version "$sig_version" \
  --arg sig_sha "$sig_sha" \
  --arg jvm_amd64 "$(sha256sum signal-cli-jvm-linux-amd64.tar.gz | cut -d' ' -f1)" \
  --arg jvm_arm64 "$(sha256sum signal-cli-jvm-linux-arm64.tar.gz | cut -d' ' -f1)" \
  '{
    version: $version,
    image: $image,
    image_digest: $digest,
    say: {
      "linux-amd64": {url: "\($base)/say-linux-amd64", sha256: $amd64},
      "linux-arm64": {url: "\($base)/say-linux-arm64", sha256: $arm64}
    },
    signal_cli: {
      version: $sig_version,
      "linux-amd64": {
        url: "https://github.com/AsamK/signal-cli/releases/download/v\($sig_version)/signal-cli-\($sig_version)-Linux-native.tar.gz",
        sha256: $sig_sha
      },
      jvm: {
        "linux-amd64": {url: "\($base)/signal-cli-jvm-linux-amd64.tar.gz", sha256: $jvm_amd64},
        "linux-arm64": {url: "\($base)/signal-cli-jvm-linux-arm64.tar.gz", sha256: $jvm_arm64}
      }
    }
  }' >manifest.json
