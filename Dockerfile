# docker build -t say .
# docker run -p 8080:8080 -v data:/data say

FROM node:24-bookworm AS web
WORKDIR /src
RUN corepack enable
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/
COPY widget/package.json widget/
COPY sdk/ts/package.json sdk/ts/
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
COPY --from=web /src/internal/api/static internal/api/static
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /say ./cmd/say \
 && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go /say /say
# A named volume copies this directory's owner, so nonroot can write to it.
COPY --from=go --chown=nonroot:nonroot /data /data
ENV SAY_DB=/data/say.db
EXPOSE 8080 6222
VOLUME /data
ENTRYPOINT ["/say"]
CMD ["serve"]
