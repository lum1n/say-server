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
