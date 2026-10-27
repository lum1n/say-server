#!/bin/sh
# release.sh [-n] VERSION does from this machine what release.yml does on
# a tag (ADR 0017), for when Actions isn't available. It tags HEAD
# vVERSION and pushes the tag, unless the tag is there already. It needs:
# - a clean tree; an existing tag vVERSION must be at HEAD;
# - docker logged in to ghcr.io with write:packages;
# - gh logged in, and jq and minisign;
# - the minisign secret key in $MINISIGN_KEY_FILE (~/.minisign/minisign.key).
# With -n it builds and signs everything into dist/ but pushes nothing.
# cosign signs the image only if it is installed.
set -eu
dry=
if [ "${1:-}" = -n ]; then dry=1; shift; fi
version=${1:?usage: release.sh [-n] VERSION, such as 0.4.1}
version=${version#v}
tag=v$version
echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "release: $version is not a version such as 0.4.1" >&2; exit 1; }
image=ghcr.io/lum1n/say-node
key=${MINISIGN_KEY_FILE:-$HOME/.minisign/minisign.key}
cd "$(dirname "$0")/.."

fail() { echo "release: $*" >&2; [ -n "$dry" ] || exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || fail "the tree has uncommitted changes"
[ -r "$key" ] || { echo "release: no minisign key at $key" >&2; exit 1; }
at=$(git rev-parse -q --verify "$tag^{commit}" || true)
if [ -z "$at" ]; then
  if [ -n "$dry" ]; then
    echo "release: would tag HEAD $tag and push it"
  else
    git tag -a "$tag" -m "say $tag"
    echo "release: tagged HEAD $tag"
  fi
elif [ "$at" != "$(git rev-parse HEAD)" ]; then
  fail "$tag is at $(git rev-parse --short "$at"), not HEAD"
fi
if [ -z "$dry" ] && ! git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null; then
  git push origin "refs/tags/$tag"
fi

echo "release: image"
docker build --target node --platform linux/amd64 --build-arg VERSION="$version" \
  --label org.opencontainers.image.source=https://github.com/lum1n/say-server \
  -t "$image:$version" .
if [ -n "$dry" ]; then
  digest=$(docker image inspect --format '{{.Id}}' "$image:$version")
else
  docker push "$image:$version"
  digest=$(docker image inspect --format '{{index .RepoDigests 0}}' "$image:$version")
  digest=${digest#*@}
  if command -v cosign >/dev/null; then
    cosign sign --yes "$image@$digest"
  else
    echo "release: no cosign, so the image is not signed"
  fi
fi

echo "release: binaries"
make web
rm -rf dist && mkdir dist
for arch in amd64 arm64; do
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o dist/say-linux-$arch ./cmd/say
done

echo "release: manifest"
scripts/manifest.sh "$version" "$digest" dist
cat dist/manifest.json
minisign -S -s "$key" -m dist/manifest.json -t "say $tag manifest"

if [ -n "$dry" ]; then
  echo "release: dry run, built into dist/ and pushed nothing"
  exit 0
fi
# The manifest goes up last, so it never names an image that isn't there.
docker tag "$image:$version" "$image:stable"
docker push "$image:stable"
gh release create "$tag" --verify-tag --generate-notes \
  dist/say-linux-amd64 dist/say-linux-arm64 dist/SHA256SUMS \
  dist/manifest.json dist/manifest.json.minisig
