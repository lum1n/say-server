# ADR 0007: Setup wizard and sign-in

Status: accepted (2026-10-24)

## Context

`say` must start with no configuration and hand the operator a browser
wizard that creates the admin. A fresh instance is often reachable from the
internet before anyone has run the wizard, and in cluster mode several nodes
can serve it at once. People then need to sign in from a browser, and
access tokens (ADR 0006) need an expiry and a way to revoke them.

## Decision

- **Setup code.** While no admin exists, each node logs a link to
  `/setup?code=…` at startup. `POST /api/v1/setup` needs that code, so
  reaching the instance first is not enough to claim it. The code is random
  per process unless `--setup-code` / `SAY_SETUP_CODE` sets it; set it on
  every node of a cluster so the link works behind a load balancer.
- **Setup happens once.** The setup transaction claims an `instance` row
  with `INSERT … ON CONFLICT DO NOTHING`, then writes the admin and its
  login. Two nodes racing both try the claim; one inserts, the other
  inserts nothing and answers 409 `setup_done`. The same statement works on
  SQLite and Postgres, so no engine-specific error codes are parsed.
- **Instance admin** is a flag on the principal, separate from room roles.
  Admins create people (username and password) and agents (a token that
  never expires) through `POST /api/v1/principals`.
- **Passwords** are hashed with argon2id at the OWASP minimum (19 MiB, two
  passes), stored as PHC strings so the parameters can be raised later
  without breaking old hashes. A login for an unknown username still checks
  a dummy hash, so timing does not reveal which usernames exist.
- **Sessions are access tokens.** Sign-in issues a token named `session`
  that expires after 30 days and sets it as the `say_token` cookie:
  HttpOnly, SameSite=Lax, and Secure when the request came over HTTPS
  (directly or via `X-Forwarded-Proto`). There is no second session
  mechanism; everything that checks a token checks sessions too. Sign-out
  deletes the token it was called with.

## Consequences

- An operator who loses the log can restart the node for a fresh code, or
  set `--setup-code`.
- Login is not rate-limited yet; argon2id makes each guess cost the
  attacker and the server alike. A per-IP and per-username limit is needed
  before public instances.
- Tokens are revoked one at a time, by the holder. Listing and revoking
  another session (or an agent's token) needs token ids in the API.
