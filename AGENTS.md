# AGENTS.md — technical reference

Single-user, mobile-first YouTube downloader: a Go backend serving an
embedded TypeScript UI. This repo holds the source and a local development
mode (`make run-dev`) — nothing more: any real deployment (e.g. exposing
the server through a Cloudflare Tunnel) is set up and managed outside the
repo. The human-facing quick start is in [README.md](README.md); this
document holds everything an agent needs to work on the repo.

## Layout

- Go 1.27 module (`personal-yt-downloader`) at the repo root: `go.mod` and
  `go.sum` at the top, with `cmd/server` (the binary) and
  `internal/app` (all logic and tests) below. No vendoring; plain module
  downloads via `go.mod`/`go.sum`.
- `web/` — TypeScript + CSS frontend, no framework; Vite builds it, vitest
  (jsdom) tests it.
- `internal/app/web/dist` — where `make embed` copies the built UI for
  `go:embed`. **Gitignored**: a plain `go build` requires `make embed` (or
  `make build`) first, which always embeds a fresh web build.
- Data directory (`./data` on the host under `make run-dev`, mounted at
  `/data` in the container, or wherever `-data` points): per video
  `videos/<id>/metadata.json` and
  `videos/<id>/video.mp4`, plus `sessions.json` and `links.json`.
- `shortcuts/` — source of the iOS Shortcut (`Download-YouTube-Video.plist`,
  Share Sheet name "Download YouTube video"): takes a URL from the Share
  Sheet or an ask prompt, POSTs it to `/api/jobs` with the Bearer token,
  notifies, and finishes — no polling or downloading (two import questions:
  API origin and token). `make shortcut` (macOS only) lints and signs an
  importable build into `shortcuts/build/`; the plist is the committed
  source of truth, the signed build is gitignored.
- `skills/` — agent skills shared by every tool: `.claude/skills` and
  `.agents/skills` are symlinks to this one directory, so Claude Code and
  other agents discover the same skills. Holds
  `download-youtube-video-shortcut/` (editing rules and the plist-wiring
  reference for the Shortcut).

## Configuration

The app reads the following environment variables:

| Variable | Where | Meaning |
| --- | --- | --- |
| `PASSWORD` | app, required | The single login password, 1–1024 characters. `make run-dev` passes `local` unless `PASSWORD` is set in the environment; a real deployment should use a strong one. Hashed (Argon2id) once at boot; never written to disk. Changing it and restarting revokes every stored session. |
| `API_TOKEN` | app, optional | When set to a non-empty value (≤1024 characters, surrounding whitespace trimmed at boot), enables `Authorization: Bearer <token>` authentication for direct API requests (curl, iOS Shortcuts) against the `/api/jobs*` endpoints. Unset or blank disables bearer auth entirely; changing it takes effect on restart. Held in memory as a SHA-256 digest, compared in constant time, never written to disk or logs. Use a long random string — the token carries full API power (submit, delete, share links). |

Everything else on the binary is a flag, not an env var: `-data` (default
`/data` — what the dev container uses, with `./data` bind-mounted over
it), `-addr` (default `:8080`),
`-insecure-cookies` (plain-HTTP local dev, used by `make run-dev`),
`-trusted-proxies` (comma-separated IPs/CIDRs whose `X-Forwarded-For`
the login rate limiter honors; default `127.0.0.0/8,::1/128`, an empty
value trusts no proxy at all; IPv4-mapped IPv6 entries mean their plain
IPv4 form). The
binary serves the app when run with these flags; its one subcommand,
`server healthcheck`, probes `127.0.0.1:8080/healthz`. There is no
set-password flow and no interactive prompt of any
kind — the password comes from the environment, full stop.

## Auth model

- One password, from `PASSWORD`, Argon2id-hashed at boot in memory.
- Bearer auth for direct API requests: setting `API_TOKEN` lets a client
  authenticate any `/api/jobs*` route with `Authorization: Bearer <token>`
  instead of a session — full API parity (list, submit, retry, delete,
  download, share links). The `/api/session` and `/api/logout` routes stay
  cookie-only; they are meaningless for a token client. Bearer requests
  skip CSRF and Origin checks (nothing cookie-shaped is involved, and a
  cross-site page cannot attach an Authorization header); the token is
  verified in constant time against a SHA-256 digest. A request carrying a
  Bearer header never falls back to cookie auth — a bad or malformed token
  (even a Bearer scheme with no token at all) is a 401 even alongside a
  valid session. Token attempts share the login limiter's per-IP budget
  (5 failures per 15 minutes block), adjudicated atomically — block check,
  credential comparison, and failure recording under one lock — so
  concurrent guesses cannot slip past the failure limit, and a blocked IP
  is rejected before its token is even examined: wrong and correct guesses
  look identical, so brute force gets no oracle for which guess landed.
  Successful attempts never touch the limiter, so a polling client never
  burns the budget. The token grants full API power — treat it like the
  password.
- Sessions: 30-day TTL, cap 20, persisted as token hashes in
  `sessions.json` alongside a fingerprint of the boot password
  (deterministic Argon2id over a fixed salt — stable across restarts under
  an unchanged password, expensive to brute-force offline). On load, a
  fingerprint mismatch (i.e. the password changed) drops all sessions.
- Cookies: `HttpOnly`, `SameSite=Strict`, `Secure` unless the
  `-insecure-cookies` dev flag is set. CSRF token echoed via `X-CSRF-Token`
  on every state-changing request, plus `Origin` validation. Login is
  rate-limited per client IP (5 failures or 30 attempts per 15 minutes
  block).
- Share links (`/m/<token>/<filename>.mp4`, where the filename is the real
  video title, percent-escaped, so URL-based saving keeps the name; the token
  alone is the credential — token-only links from before this shape still
  resolve): unguessable tokens, 24-hour expiry, bound to one video incarnation
  (`CreatedAt`), revoked on delete or retry. Invalid, expired, and revoked
  links are indistinguishable 404s; tokens never appear in logs.

## Job model

- Stages: `queued` → `downloading.video` / `downloading.audio` →
  `processing` → `verifying` → `ready` / `failed`.
- One download worker (the server never runs concurrent downloads); a pool
  of 2 title fetchers so titles land early; at most 16 unfinished jobs —
  submissions beyond that get 429.
- Titles persist through download failures and restarts: in-flight jobs
  recover on boot as failed-but-retryable with their titles intact; partial
  download files are cleaned up at startup (`CleanWorkFiles`).
- Playlists, live streams, and upcoming premieres are rejected up front.

## Media pipeline

yt-dlp downloads (separate video+audio format selection), then ffmpeg
remuxes/processes to H.264 + AAC + `yuv420p`, ≤720p in either orientation,
with the `moov` atom at the front (fast start). ffprobe verifies the result
before the job turns `ready`. yt-dlp's format-selector grammar does not allow
`&` inside one bracket group — stack bracket groups instead.

The dev image (`make run-dev` builds it) bundles the tools — pinned yt-dlp
(pip, installed with its `[default]` extra so the matching `yt-dlp-ejs`
script package comes along) + ffmpeg from Debian, plus deno (pinned 2.9.6):
current yt-dlp needs a JavaScript runtime and the EJS scripts for full
YouTube extraction; without them some formats go missing and some videos
fail with "no matching format". Nothing needs to be installed on the
host; override the pins with
`docker build --build-arg YT_DLP_VERSION=<v> --build-arg DENO_VERSION=<v>`.

## Build pipeline

- `make build` = web build (npm install + vite build) → `make embed`
  (copy `web/dist` into `internal/app/web/dist`) → `go build`.
- `make test` = `go vet` + `go test -race` + vitest. `embed` depends on the
  web build, so `go-build` and `go-test` always embed a fresh dist (the
  dist is gitignored).
- Docker (`Dockerfile`, context = repo root, trimmed by `.dockerignore`):
  node web build → golang build → deno stage → debian:trixie-slim runtime
  with pinned yt-dlp, ffmpeg, non-root `app` user, healthcheck. The image
  build runs no tests; `make test` is the only place they run.
- `make run-dev` = `docker-build` (tag `personal-yt-downloader`), then one
  `docker run --rm`: port `8080` published, `PASSWORD=local` unless
  overridden via the environment, `API_TOKEN` passed through from the
  environment (empty means bearer auth stays off), `./data` bind-mounted at
  `/data`, `-insecure-cookies` for plain HTTP; Ctrl-C stops it. Deploying
  for real — fronting proxy, tunnel — is handled outside the repo.

## Frontend notes (iOS Safari realities)

- Swipe-to-delete (`web/src/swipe.ts`) uses touch and mouse events directly —
  pointer events proved unreliable on iOS Safari. Releasing a swipe past the
  halfway point opens the delete confirmation and the surface springs back
  to rest on its own; there is no delete button on cards — the swipe is the
  only delete path (a cancelled gesture settles back without prompting).
  Vertical movement is never claimed (page keeps scrolling);
  `touch-action: pan-y` on the surface.
- Copy-link (`web/src/main.ts`) hands the clipboard a `ClipboardItem` holding
  a promise: iOS Safari drops the tap's user-gesture activation across the
  fetch, so the write must start inside the gesture and resolve later.
  Fallbacks: fetch-then-`writeText`, then the `execCommand` textarea trick.
- The submit modal does NOT auto-submit on paste — paste only fills the
  field; the Add button or Enter submits.
- `style.css` has a global `[hidden] { display: none !important }` — flex
  containers (`.actions`, `.chip`) would otherwise override the attribute.
  Download/copy actions render below the card content, right-aligned, only
  once the job is `ready`.
- The list polls every 2 seconds; an offline banner appears when requests
  fail and the list recovers on reconnect.

## Security posture

- Argon2id password hashing (in memory only); sessions and share links
  stored as hashes, never plaintext tokens. The optional `API_TOKEN` is
  likewise held as a SHA-256 digest and compared in constant time; login
  and bearer attempts are adjudicated by the shared per-IP limiter in one
  atomic step — check, verify, record under a single lock, so concurrent
  guesses cannot slip past the failure limit — and a blocked IP is turned
  away before its token is even examined, while valid token attempts
  leave the limiter untouched.
- CSRF tokens + `Origin` checks + `SameSite=Strict`; hardened response
  headers (`nosniff`, `no-referrer`, `DENY`, `no-store` off `/assets/`).
- The server listens on `:8080` (all interfaces — for local dev that means
  plain-HTTP dev cookies are visible to the LAN, so run `make run-dev` on
  a trusted network). `X-Forwarded-For` is honored only from peers listed
  in `-trusted-proxies` (loopback by default; an empty list trusts no
  proxy), and even then the header is walked right to left — skipping
  trusted proxies, stopping at the first malformed entry — because the
  leftmost entry is client-controlled. Direct clients, whether from the
  LAN or over Tailscale (`100.64.0.0/10` is never implicitly trusted),
  are keyed by their own address with no configuration needed. Fronting
  the server with a proxy on another host (e.g. cloudflared in a compose
  network, managed outside the repo): pin the proxy's specific address (a
  static `ipv4_address`) rather than the whole compose subnet, which
  would trust every container in it. The login limit stays per-IP, so
  clients sharing one NAT or CGNAT address share a counter.
- API bodies are capped (8 KiB), single JSON object, unknown fields
  rejected.
- The dev run logs in with a deliberately trivial password (`local`) over
  plain HTTP — fine on a trusted LAN, but a real deployment (outside this
  repo) should set a strong `PASSWORD` and sit behind HTTPS.

## Notes and limits

- Individual videos only; no duration/size/job-time limits — the queue cap
  (16 unfinished) is the only throttle.
- Removing a video removes its directory and revokes its links. Deleting
  and resubmitting never reactivates an old link (incarnation binding).
- `writeFileSync` persists stores atomically (temp file + rename) with
  0600 perms.
