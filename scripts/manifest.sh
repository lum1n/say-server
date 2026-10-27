#!/bin/sh
# manifest.sh VERSION IMAGE_DIGEST DIR writes DIR/SHA256SUMS and
# DIR/manifest.json for the release vVERSION (ADR 0017). DIR holds
# say-linux-amd64 and say-linux-arm64. Both release.yml and release.sh
# use it, so the two make the same manifest.
set -eu
version=$1 digest=$2 dir=$3
root=$(cd "$(dirname "$0")/.." && pwd)
repo=${GITHUB_REPOSITORY:-lum1n/say-server}
base="https://github.com/$repo/releases/download/v$version"
# signal-cli comes from its own releases, pinned in the Dockerfile.
sig_version=$(sed -n 's/^ARG SIGNAL_CLI_VERSION=//p' "$root/Dockerfile")
sig_sha=$(sed -n 's/^ARG SIGNAL_CLI_SHA256=//p' "$root/Dockerfile")

cd "$dir"
sha256sum say-linux-amd64 say-linux-arm64 >SHA256SUMS
jq -n \
  --arg version "$version" \
  --arg image "ghcr.io/lum1n/say-node:$version" \
  --arg digest "$digest" \
  --arg base "$base" \
  --arg amd64 "$(sha256sum say-linux-amd64 | cut -d' ' -f1)" \
  --arg arm64 "$(sha256sum say-linux-arm64 | cut -d' ' -f1)" \
  --arg sig_version "$sig_version" \
  --arg sig_sha "$sig_sha" \
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
      }
    }
  }' >manifest.json
