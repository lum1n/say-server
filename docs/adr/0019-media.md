# ADR 0019: Media: images and other files

Status: accepted (2026-10-28)

## Context

say was text-only. The `attachment` part kind was in the protocol from
the start, but nothing made one, and bridges dropped what networks sent
besides text. People send photos on Signal, Telegram and the rest every
day, and a bridged room that loses them is not the same conversation.
Files need somewhere to live, a way to get them into a message, a way
for browsers to show them, and a way through bridges both ways.

## Decision

- **Bytes go in a media store, facts in the database.** `media.Store`
  is a small interface (put, open, delete by id). The one say ships keeps
  files on local disk, under `--media-dir` (`SAY_MEDIA_DIR`), by default
  `media` beside the SQLite file, or `data/media` with Postgres. Nodes of
  a cluster need that directory shared until media can live in object
  storage, which only needs another `Store`.
- **A `media` table** records each file: its room, its uploader, type,
  name, size, image width and height, and SHA-256. A file belongs to the
  room it was uploaded to.
- **Upload first, then send.** `POST /rooms/{id}/media` takes the raw file
  (members who can send; observers can't) and returns an `Attachment`.
  A message names it in an `attachment` part by its `media` id. On send
  and on edit, say checks each attachment part: the file must be the
  sender's own upload to the same room, else 422, and say rewrites the
  part's data from the media row, whatever the client wrote. A message
  has at most 10 attachments and may have nothing else.
- **say decides what a file is.** It sniffs the type. An image must be the
  image type it says it is, and a missing type takes the sniffed one.
  HTML, SVG, XML and JavaScript are refused (415), since a browser would
  run them as a page on say's origin. Width and height come from the
  image headers of JPEG, PNG, GIF and WebP; an image whose header can't
  be read is refused too. A file is at most 25 MiB by default,
  `--media-max` (`SAY_MEDIA_MAX`, in MiB); past it is 413.
- **Two ways to fetch a file.** Members fetch
  `GET /rooms/{id}/media/{media_id}` with their token. Attachment parts
  in what say returns also carry a `url`: a signed
  `/api/v1/media/{id}?exp=…&sig=…` that needs no token, for `<img>` and
  the like. The HMAC key is kept in the instance table, so every node
  signs alike. Expiry is rounded: a URL works for 24 to 36 hours and
  stays the same for 12 hours at a time, so clients can cache what it
  serves.
- **Served files can't run.** Images are served `inline`, everything else
  as an `attachment` to download. Every file gets
  `X-Content-Type-Options: nosniff` and
  `Content-Security-Policy: sandbox; default-src 'none'`.
- **Bridges, coming in.** A driver puts a message's files in
  `Incoming.Media`, each with an `Open` that reads it. say fetches and
  keeps each as it takes the message in, as the ghost that sent it. A
  file it can't fetch or keep (too large, a refused type, a network
  error) becomes a line of text, `[image: name]` or `[file: name]`, so
  the message still says something was sent.
- **Bridges, going out.** On a network with the `attachments` capability,
  `Outgoing.Media` carries the message's files, each opened from the
  store when the driver sends it. Elsewhere, such as IRC, the text gets
  the same `[image: name]` placeholder lines instead.
- **Webhooks carry data, never URLs.** A file posted to a hook comes as
  base64 in the body. say fetches nothing a webhook names, so a hook
  can't make the server reach into its own network.

## Consequences

- Files uploaded but never sent stay on disk and in the table. There is
  no garbage collection yet; it is left for later.
- No thumbnails: clients get the whole image and scale it.
- The web client is still text-only.
- A cluster without a shared media directory serves a file only from the
  node that took it.
- A signed URL that leaks works for anyone until it expires, up to 36
  hours.
