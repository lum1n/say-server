# ADR 0017: Your own node, deployed to exe.dev from the app

Status: accepted (2026-10-27)

## Context

The apps should not keep people's messages on a server we run, and much
of say can't live in a phone app. signal-cli is a subprocess, which iOS
doesn't allow, and its libraries are AGPL while say is Apache-2.0.
Bridges inside an app also stop when the app is suspended, so nothing is
received in the background and no notifications arrive, on any network.

So each person gets **their own always-on say node**, set up from the
app in one step. The phone is a synced client of that node (ADR 0015).
It still works on its own for networks that can run in the app.

exe.dev is the first host. Each person has their own exe.dev account,
and the node runs in it. That fits exe.dev's terms (no reselling), and we
never hold anyone's messages. The same approach works in `../nudge` (its
ADR 0011) and `../sessh-swift`.

## Decision

- **Signing in to exe.dev needs no computer.** The app makes its own
  Ed25519 key, which stays in the Keychain or Keystore. It opens
  `ssh exe.dev` with that key and shows exe.dev's sign-in screen, kept by
  a terminal emulator (`charmbracelet/x/vt`) and drawn natively with a
  text field for input. The person types their email and opens the link
  exe.dev mails them, which adds the key to their account, new or
  existing. The app renders whatever exe.dev shows and matches no
  prompts, so a changed screen keeps working.
  - exe.dev's host key is pinned (`exe.HostKey`, from its host-key FAQ).
    exe.dev presents it inside an SSH host certificate, and the pin is on
    the certified key.
- **The token is minted on the device.** An exe0 token is the
  permissions JSON and its SSHSIG signature (namespace `v0@exe.dev`,
  SHA-512), each base64url. The app mints one from its key at once and
  tries `whoami` every 3 s; when exe.dev takes it, the person is signed
  in. This also works if the link was opened while the app was in the
  background.
  - The token may run only `whoami, ls, new, rm, restart, share show,
    share port, share set-public`.
  - It lasts 90 days and is reminted when less than 14 days are left.
    That needs no one, because the key is on the device.
  - People who would rather not sign in this way can paste a token made
    with `ssh exe.dev ssh-key generate-api-key` (`exe.APIKeyCommand`).
    A pasted token isn't reminted.
- **Deploying is host-neutral except for one step.** Package
  `say/deploy` does the shared part:
  - picks a name (`say-<random>`) and a setup code (Crockford base 32);
  - looks up the newest release's image (`ghcr.io/lum1n/say-node:<version>`)
    in the release manifest, falling back to `:stable`. The exact version
    is pinned because a host may keep an image it pulled by its tag;
  - waits for `GET /api/v1/setup` to say the node is open;
  - claims it with `POST /api/v1/setup`.
  Only making and removing the machine differ between hosts, behind
  `deploy.Host` (`Create`, `Delete`). Signing in is per host and outside
  that interface. `say/exe` is the first host.
- **exe.dev commands go through its HTTPS API**, `POST https://exe.dev/exec`
  with the command line as the body:
  `new --json --no-email --name … --image … --memory 2GB --disk 20GB
  --env SAY_SETUP_CODE=… --env SAY_SETUP_MINUTES=30`, then
  `share port <name> 80` and `share set-public <name>`. say's own sign-in
  guards the node, not an exe.dev login.
  - `/exec` takes a raw command line and nothing is quoted, so every
    value is checked against a strict pattern before it is sent.
  - The output of `new` and `ls` isn't documented, so it is read
    leniently, with `https://<name>.exe.xyz` as the fallback address.
- **Setup windows.** `say serve --setup-minutes` (`SAY_SETUP_MINUTES`)
  closes setup that long after start if nobody claimed the node. The
  setup code is then refused (`403 setup_closed`), and `GET /setup` says
  `open: false`, so the app offers to remove the node. Before this, a
  setup code worked until an admin existed (ADR 0007).
- **One implementation for both apps.** `mobile/` binds all of it with
  gomobile: `NewExeKey`, `MintExeToken`, `StartExeLogin` (with an
  `ExeScreen` callback: `OnScreen`, `OnSignedIn`, `OnClose`), `Exe`
  (`Whoami`, `Nodes`, `Create`, `WaitOpen`, `Delete`, `Cancel`) and
  `ClaimNode`. `Create` and `WaitOpen` are separate calls because a
  gomobile call that fails returns no value. The app has to know a VM's
  name before waiting on it, so it can offer to delete a node that never
  opens.
- **`say exe`** (`login`, `whoami`, `ls`, `deploy`, `rm`, `logout`) runs
  the same flow from a terminal, before the apps exist. Its key and token
  are kept in the user's config directory.
- **Tests run against a stand-in exe.dev** (`say/exe/exetest`):
  - `/exec` checks exe0 signatures, expiry and command scope, as exe.dev
    does;
  - an SSH sign-in with a host certificate asks for an email and then
    adds the key to the account;
  - `new` starts a real say node in process.
  The SSHSIG is also checked with OpenSSH's `ssh-keygen -Y
  check-novalidate`.

- **The say-node image** is the Dockerfile's `node` target, because
  exe.dev boots an image as a VM and doesn't document how it treats a
  CMD that isn't an init:
  - Debian with systemd as PID 1, as in exe.dev's own exeuntu. A small
    `init` mounts cgroup2 where a container lacks it, then runs systemd.
  - `say.service` runs `say serve` as user `say`, on port 80 (`EXPOSE 80`,
    which exe.dev's proxy prefers), with its data and the bridges' files
    in `/var/lib/say`.
  - systemd gives services none of the host's settings. exe.dev writes
    `new --env` to `/etc/profile.d/exe-env.sh` and Docker puts `-e` in PID
    1's environment, so before say starts, `say-env` copies the `SAY_*`
    variables from both into `/run/say.env` (root only), which the unit
    reads.
  - The root filesystem grows to the VM's disk on first boot, and every
    VM makes its own machine ID.
  - It is about 170 MB compressed; signal-cli's native build is most of
    it.
  - `make node-test` boots it under Docker and claims it, in CI too.
- **say trusts the proxy's host.** `say serve --trust-proxy`
  (`SAY_TRUST_PROXY`, on in the image) takes the host from
  `X-Forwarded-Host`, so the WebSocket's Origin check sees the address
  people use. A browser on another site can't set that header, so a
  node reached directly is no worse off. say already took
  `X-Forwarded-Proto` for secure cookies.
- **Releases.** A `v*` tag builds and pushes
  `ghcr.io/lum1n/say-node:<version>` (amd64, like signal-cli's native
  build and exe.dev) with a keyless cosign signature. It builds `say`
  for Linux, then publishes `manifest.json` last, so the manifest never
  names a missing image, and moves `:stable` to the same digest. The
  manifest gives the version, image and digest, each binary's URL and
  SHA-256, and signal-cli's pinned version and checksum, for
  `say-update`. It is signed with minisign (`manifest.json.minisig`).
  An Ed25519 key that say pins is simple to check from Go, unlike
  sigstore bundles.

## Consequences

- Sign-in depends on exe.dev's SSH sign-up screen. A change there could
  need an app update, though the generic terminal makes that less likely.
- A node's self-update, device tokens and pairing, the push gateway, and
  encrypted backups are later phases of this design and aren't built
  yet. Until
  device tokens exist, `ClaimNode` returns a 30-day session token.
- Releasing needs a `MINISIGN_KEY` secret (an unencrypted minisign
  secret key), and the `say-node` package on ghcr must be public:
  exe.dev pulls the image itself, before the VM exists.
- Other hosts (Hetzner, Fly, a home box with Docker) need only a
  `deploy.Host` and a way to sign in. The image and the claim flow stay
  the same.
- The SSH client and the vt emulator add about 0.8 MB to the stripped
  `say` binary (59.0 → 59.8 MB).
