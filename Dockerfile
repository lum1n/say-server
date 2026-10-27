# docker build -t say .
# docker run -p 7290:7290 -v data:/data say

# The web and Go stages run on the build machine and cross-compile, so an
# arm64 image builds without emulation.
FROM --platform=$BUILDPLATFORM node:24-bookworm AS web
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

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY say say
COPY --from=web /src/internal/api/static internal/api/static
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN export CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
 && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /say ./cmd/say \
 && go build -trimpath -ldflags "-s -w" -o /signal-cli-jvm ./cmd/signal-cli-jvm \
 && mkdir /data

# signal-cli for the Signal bridge. Signal's servers turn away old
# versions, so keep it current (and LIBSIGNAL_VERSION at the libsignal it
# ships with). On amd64 it is the native build, which needs no Java.
# Elsewhere it is the JVM build with a JRE: its jar carries libsignal only
# for amd64, so libsignal_jni.so for the image's architecture comes from
# github.com/exquo/signal-libs-build. SIGNAL_CLI_BUILD=jvm takes the JVM
# build on amd64 too; SIGNAL_CLI_VERSION="" leaves signal-cli out.
FROM eclipse-temurin:25-jre AS jre
FROM debian:bookworm-slim AS target
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS signal
ARG TARGETARCH
ARG SIGNAL_CLI_VERSION=0.14.9
ARG SIGNAL_CLI_SHA256=a8fab78b5e8053abf3db5725272957ad11d1745b9a16848ddb2ace4743dfa5d5
ARG SIGNAL_CLI_JVM_SHA256=c32b87f587198cbd277c9d71228716ee6047ee2b63e8e88728e8280d9407d65a
ARG LIBSIGNAL_VERSION=0.103.0
ARG LIBSIGNAL_AMD64_SHA256=83ef0f4e2fa269fc157211f58db9ebaec10ff5f0a64226c34339e6e4216fab16
ARG LIBSIGNAL_ARM64_SHA256=bc112187bef1989ac5a3de14a0e43c53ba6c0c7fbc20b97cc363e92a7012728f
ARG SIGNAL_CLI_BUILD
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl zip
COPY --from=target /usr/lib/*-linux-gnu/libz.so.1* /tmp/libz/
COPY --from=jre /opt/java/openjdk /tmp/jre
COPY --from=go /signal-cli-jvm /tmp/
RUN set -e; mkdir /out; \
    [ -n "$SIGNAL_CLI_VERSION" ] || exit 0; \
    build="$SIGNAL_CLI_BUILD"; \
    [ -n "$build" ] || { [ "$TARGETARCH" = amd64 ] && build=native || build=jvm; }; \
    release="https://github.com/AsamK/signal-cli/releases/download/v${SIGNAL_CLI_VERSION}"; \
    mkdir -p /out/usr/local/bin; \
    if [ "$build" = native ]; then \
      [ "$TARGETARCH" = amd64 ] || { echo "signal-cli's native build is amd64 only" >&2; exit 1; }; \
      curl -fsSL -o /tmp/signal-cli.tgz "$release/signal-cli-${SIGNAL_CLI_VERSION}-Linux-native.tar.gz"; \
      echo "${SIGNAL_CLI_SHA256}  /tmp/signal-cli.tgz" | sha256sum -c -; \
      tar -xzf /tmp/signal-cli.tgz -C /out/usr/local/bin signal-cli; \
      mkdir -p /out/usr/lib/x86_64-linux-gnu; \
      cp -L /tmp/libz/libz.so.1 /out/usr/lib/x86_64-linux-gnu/; \
      mkdir -p /out/usr/local/lib/say; \
      echo "$SIGNAL_CLI_VERSION" >/out/usr/local/lib/say/signal-cli.version; \
    else \
      case "$TARGETARCH" in \
        amd64) triple=x86_64-unknown-linux-gnu; sum="$LIBSIGNAL_AMD64_SHA256";; \
        arm64) triple=aarch64-unknown-linux-gnu; sum="$LIBSIGNAL_ARM64_SHA256";; \
        *) echo "no libsignal for $TARGETARCH" >&2; exit 1;; \
      esac; \
      curl -fsSL -o /tmp/signal-cli.tgz "$release/signal-cli-${SIGNAL_CLI_VERSION}.tar.gz"; \
      echo "${SIGNAL_CLI_JVM_SHA256}  /tmp/signal-cli.tgz" | sha256sum -c -; \
      curl -fsSL -o /tmp/libsignal.tgz "https://github.com/exquo/signal-libs-build/releases/download/libsignal_v${LIBSIGNAL_VERSION}/libsignal_jni.so-v${LIBSIGNAL_VERSION}-${triple}.tar.gz"; \
      echo "${sum}  /tmp/libsignal.tgz" | sha256sum -c -; \
      tar -xzf /tmp/signal-cli.tgz -C /tmp; \
      mkdir -p /out/opt/signal-cli; \
      mv "/tmp/signal-cli-${SIGNAL_CLI_VERSION}/lib" /out/opt/signal-cli/; \
      zip -q -d "/out/opt/signal-cli/lib/libsignal-client-${LIBSIGNAL_VERSION}.jar" '*.so' '*.dylib' '*.dll'; \
      tar -xzf /tmp/libsignal.tgz -C /out/opt/signal-cli/lib libsignal_jni.so; \
      cp -a /tmp/jre /out/opt/java; \
      cp /tmp/signal-cli-jvm /out/usr/local/bin/signal-cli; \
    fi

# The say-node image (docker build --target node), for your own node on a
# host such as exe.dev (ADR 0017). The host boots it as a VM, so systemd is
# PID 1 and runs say on port 80 behind the host's HTTPS proxy, with its data
# in /var/lib/say. say-update.timer keeps say and signal-cli current, since
# a new image doesn't reach a VM that already exists. Test it locally with
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
# signal-cli's native build (the image is amd64), and the version file
# say-update reads to tell whether a release brings a new signal-cli.
COPY --from=signal /out/ /
COPY image/node/init /usr/local/bin/init
COPY image/node/say-env /usr/local/lib/say/say-env
COPY image/node/say.service image/node/say-update.service image/node/say-update.timer \
     image/node/say-update.path /etc/systemd/system/
COPY image/node/growfs.conf /etc/systemd/system/systemd-growfs@-.service.d/say.conf
RUN systemctl enable say.service say-update.timer say-update.path \
 # Each VM makes its own machine ID on first boot. Empty, not removed: an
 # absent one counts as a first boot and presets every unit again.
 && : >/etc/machine-id
EXPOSE 80
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/local/bin/init"]

# cc rather than static: signal-cli links against glibc (and zlib, copied
# in), as do the JRE and libsignal.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=go /say /say
COPY --from=signal /out/ /
# A named volume copies this directory's owner, so nonroot can write to it.
COPY --from=go --chown=nonroot:nonroot /data /data
ENV SAY_DB=/data/say.db
# Bridges keep their files (such as signal-cli's keys) under here.
ENV XDG_DATA_HOME=/data
EXPOSE 7290 6222
VOLUME /data
ENTRYPOINT ["/say"]
CMD ["serve"]
