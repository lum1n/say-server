#!/bin/sh
# Boots the say-node image as a host would and checks that systemd starts
# say on port 80 with the host's settings: the setup code it was given
# opens setup, and nothing failed to start. Then it updates the node to a
# release signed with a throwaway key and served from another container,
# and checks that say asking for an update starts the updater. It needs
# go, jq and minisign.
set -eu
image=${1:-say-node:latest}
name=say-node-test-$$
tmp=$(mktemp -d)
trap 'docker rm -f "$name" "$name-files" >/dev/null 2>&1; docker network rm "$name" >/dev/null; rm -rf "$tmp"' EXIT
fail() {
	echo "node-test: $*" >&2
	exit 1
}
docker network create "$name" >/dev/null
docker run -d --privileged --name "$name" --network "$name" -p 127.0.0.1::80 \
	-e SAY_SETUP_CODE=NODETEST1 -e SAY_SETUP_MINUTES=5 "$image" >/dev/null
url=http://$(docker port "$name" 80/tcp | head -n1)
i=0
until curl -fsS "$url/api/v1/setup" 2>/dev/null | grep -q '"open":true'; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		docker logs "$name"
		fail "setup never opened"
	fi
	sleep 1
done
status=$(curl -s -o /dev/null -w '%{http_code}' -c "$tmp/cookies" -X POST "$url/api/v1/setup" \
	-H 'content-type: application/json' \
	-d '{"code":"NODETEST1","username":"ada","password":"node test password 1"}')
[ "$status" = 201 ] || fail "claiming with the setup code gave $status"
state=$(docker exec "$name" systemctl is-system-running || true)
if [ "$state" != running ]; then
	docker exec "$name" systemctl --failed
	fail "system is $state"
fi
for unit in say-update.timer say-update.path; do
	[ "$(docker exec "$name" systemctl is-active "$unit")" = active ] || fail "$unit isn't active"
done
version=$(curl -fsS "$url/api/v1/health" | jq -r .version)

# A release of a say that says it is 99.0.0, made the way
# scripts/release.sh makes one, signed with a throwaway key and served
# from another container.
mkdir "$tmp/release"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=99.0.0" \
	-o "$tmp/release/say-linux-amd64" ./cmd/say
cp "$tmp/release/say-linux-amd64" "$tmp/release/say-linux-arm64"
scripts/manifest.sh 99.0.0 sha256:test "$tmp/release"
files=http://$name-files:8000
jq --arg base "$files" '.say[].url |= ($base + "/" + (split("/") | last))' \
	"$tmp/release/manifest.json" >"$tmp/manifest.json"
mv "$tmp/manifest.json" "$tmp/release/manifest.json"
minisign -G -W -p "$tmp/test.pub" -s "$tmp/test.key" >/dev/null
minisign -S -s "$tmp/test.key" -m "$tmp/release/manifest.json" >/dev/null
chmod -R a+rX "$tmp"
docker run -d --name "$name-files" --network "$name" -v "$tmp/release:/srv:ro" \
	busybox httpd -f -p 8000 -h /srv >/dev/null
until docker exec "$name-files" wget -q -O /dev/null http://localhost:8000/manifest.json; do sleep 1; done
docker cp "$tmp/test.pub" "$name:/tmp/test.pub"
docker exec "$name" say update --manifest "$files/manifest.json" --key /tmp/test.pub \
	|| { docker exec "$name" journalctl -u say --no-pager | tail -20; fail "say update failed"; }
node=$(curl -fsS -b "$tmp/cookies" "$url/api/v1/node")
echo "$node" | jq -e --arg from "$version" \
	'.version == "99.0.0" and .update.state == "ok" and .update.updated_from == $from and (.update.available | not)' >/dev/null \
	|| fail "after the update, GET /node = $node"
curl -fsS "$url/api/v1/setup" | grep -q '"needed":false' || fail "the update lost the admin"

# say asks; systemd runs the updater, which removes the request. It checks
# GitHub's newest release, which may fail; that it ran is what counts.
status=$(curl -s -o /dev/null -w '%{http_code}' -b "$tmp/cookies" -X POST "$url/api/v1/node/update")
[ "$status" = 202 ] || fail "POST /node/update gave $status"
i=0
while docker exec "$name" test -e /var/lib/say/update-request; do
	i=$((i + 1))
	[ "$i" -lt 30 ] || fail "say-update.path never started the updater"
	sleep 1
done
echo "node-test: ok"
