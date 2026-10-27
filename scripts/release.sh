#!/bin/sh
# release.sh [-n] VERSION does from this machine what release.yml does on
# a tag (ADR 0017), for when Actions isn't available. It needs:
# - the tag vVERSION at HEAD, pushed to origin, and a clean tree;
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
image=ghcr.io/lum1n/say-node
key=${MINISIGN_KEY_FILE:-$HOME/.minisign/minisign.key}
cd "$(dirname "$0")/.."

fail() { echo "release: $*" >&2; [ -n "$dry" ] || exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || fail "the tree has uncommitted changes"
[ "$(git rev-parse -q --verify "$tag^{commit}" || true)" = "$(git rev-parse HEAD)" ] \
  || fail "$tag is not at HEAD (git tag $tag)"
git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null || fail "$tag is not on origin (git push origin $tag)"
[ -r "$key" ] || { echo "release: no minisign key at $key" >&2; exit 1; }

echo "release: image"
docker build --target node --platform linux/amd64 --build-arg VERSION="$version" \
  --label org.opencontainers.image.source=https://github.com/lum1n/say-v2 \
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
