#!/bin/sh
# Boots the say-node image as a host would and checks that systemd starts
# say on port 80 with the host's settings: the setup code it was given
# opens setup, and nothing failed to start.
set -eu
image=${1:-say-node:latest}
name=say-node-test-$$
docker run -d --privileged --name "$name" -p 127.0.0.1::80 \
	-e SAY_SETUP_CODE=NODETEST1 -e SAY_SETUP_MINUTES=5 "$image" >/dev/null
trap 'docker rm -f "$name" >/dev/null' EXIT
url=http://$(docker port "$name" 80/tcp | head -n1)
i=0
until curl -fsS "$url/api/v1/setup" 2>/dev/null | grep -q '"open":true'; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		docker logs "$name"
		echo "node-test: setup never opened" >&2
		exit 1
	fi
	sleep 1
done
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url/api/v1/setup" \
	-H 'content-type: application/json' \
	-d '{"code":"NODETEST1","username":"ada","password":"node test password 1"}')
if [ "$status" != 201 ]; then
	echo "node-test: claiming with the setup code gave $status" >&2
	exit 1
fi
state=$(docker exec "$name" systemctl is-system-running || true)
if [ "$state" != running ]; then
	docker exec "$name" systemctl --failed
	echo "node-test: system is $state" >&2
	exit 1
fi
echo "node-test: ok"
