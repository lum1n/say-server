# docker build -t say .
# docker run -p 7290:7290 -v data:/data say

FROM node:24-bookworm AS web
WORKDIR /src
RUN corepack enable
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/
COPY widget/package.json widget/
COPY sdk/ts/package.json sdk/ts/
COPY examples/echo-bot/package.json examples/echo-bot/
RUN pnpm install --frozen-lockfile
COPY Makefile ./
COPY web web
COPY widget widget
COPY sdk/ts sdk/ts
COPY protocol protocol
COPY internal/api/static internal/api/static
RUN make web

FROM golang:1.27-bookworm AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY say say
COPY --from=web /src/internal/api/static internal/api/static
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /say ./cmd/say \
 && mkdir /data

# signal-cli for the Signal bridge: its native build, which needs no Java.
# Signal's servers turn away old versions, so keep this current. The
# native build is amd64 only; elsewhere, or with SIGNAL_CLI_VERSION="",
# the image has no signal-cli.
FROM debian:bookworm-slim AS signal
ARG TARGETARCH
ARG SIGNAL_CLI_VERSION=0.14.9
ARG SIGNAL_CLI_SHA256=a8fab78b5e8053abf3db5725272957ad11d1745b9a16848ddb2ace4743dfa5d5
RUN mkdir -p /out/bin /out/lib \
 && if [ -n "$SIGNAL_CLI_VERSION" ] && [ "${TARGETARCH:-amd64}" = amd64 ]; then \
      apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
      && curl -fsSL -o /tmp/signal-cli.tgz "https://github.com/AsamK/signal-cli/releases/download/v${SIGNAL_CLI_VERSION}/signal-cli-${SIGNAL_CLI_VERSION}-Linux-native.tar.gz" \
      && echo "${SIGNAL_CLI_SHA256}  /tmp/signal-cli.tgz" | sha256sum -c - \
      && tar -xzf /tmp/signal-cli.tgz -C /out/bin signal-cli \
      && cp -L /usr/lib/x86_64-linux-gnu/libz.so.1 /out/lib/; \
    fi

# The say-node image (docker build --target node), for your own node on a
# host such as exe.dev (ADR 0017). The host boots it as a VM, so systemd is
# PID 1 and runs say on port 80 behind the host's HTTPS proxy, with its data
# in /var/lib/say. Test it locally with
#   docker run --privileged -p 8080:80 -e SAY_SETUP_CODE=… say-node
FROM debian:bookworm-slim AS node
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --home-dir /var/lib/say --shell /usr/sbin/nologin say \
 # A VM has nothing to log in on, no hardware to probe, and its host sets
 # up the network and mounts.
 && systemctl mask -- getty.target console-getty.service systemd-firstboot.service \
      systemd-udevd.service systemd-udev-trigger.service systemd-modules-load.service \
      systemd-remount-fs.service -.mount systemd-random-seed.service \
      apt-daily.timer apt-daily-upgrade.timer dpkg-db-backup.timer e2scrub_all.timer \
 && mkdir -p /etc/systemd/journald.conf.d \
 && printf '[Journal]\nStorage=persistent\nSystemMaxUse=200M\n' >/etc/systemd/journald.conf.d/say.conf \
 && systemctl set-default multi-user.target \
 # Grows the root filesystem to the VM's disk on first boot, as exeuntu does.
 && echo '/dev/vda / ext4 defaults,x-systemd.growfs 0 1' >/etc/fstab
COPY --from=go /say /usr/local/bin/say
COPY --from=signal /out/bin/ /usr/local/bin/
COPY image/node/init /usr/local/bin/init
COPY image/node/say-env /usr/local/lib/say/say-env
COPY image/node/say.service /etc/systemd/system/say.service
COPY image/node/growfs.conf /etc/systemd/system/systemd-growfs@-.service.d/say.conf
RUN systemctl enable say.service \
 # Each VM makes its own machine ID on first boot. Empty, not removed: an
 # absent one counts as a first boot and presets every unit again.
 && : >/etc/machine-id
EXPOSE 80
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/local/bin/init"]

# cc rather than static: signal-cli links against glibc (and zlib, copied in).
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=go /say /say
COPY --from=signal /out/bin/ /usr/local/bin/
COPY --from=signal /out/lib/ /usr/lib/x86_64-linux-gnu/
# A named volume copies this directory's owner, so nonroot can write to it.
COPY --from=go --chown=nonroot:nonroot /data /data
ENV SAY_DB=/data/say.db
# Bridges keep their files (such as signal-cli's keys) under here.
ENV XDG_DATA_HOME=/data
EXPOSE 7290 6222
VOLUME /data
ENTRYPOINT ["/say"]
CMD ["serve"]
