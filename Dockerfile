# syntax=docker/dockerfile:1

# The build context is the repository root (trimmed by .dockerignore), so
# the web stage can compile the UI and the Go stage can embed it.

ARG YT_DLP_VERSION=2026.8.19

# --- web: compile the TypeScript/CSS UI -------------------------------------
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- build: compile the server with the real UI -------------------------------
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ cmd/
COPY internal/ internal/
# The embedded dist must come only from the web stage above; the host copy
# is excluded via .dockerignore so a stale UI can never ship inside the binary.
COPY --from=web /src/web/dist internal/app/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- deno: the JavaScript runtime yt-dlp needs for YouTube extraction -------
# Without it, yt-dlp warns that "some formats may be missing" and old or
# unusual videos can fail with "no matching format".
FROM debian:trixie-slim AS deno
ARG DENO_VERSION=2.9.6
ARG TARGETARCH
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl unzip \
	&& rm -rf /var/lib/apt/lists/* \
	&& case "${TARGETARCH}" in \
		arm64) D=deno-aarch64-unknown-linux-gnu ;; \
		*) D=deno-x86_64-unknown-linux-gnu ;; \
	   esac \
	&& curl -fsSL -o /tmp/deno.zip \
		"https://github.com/denoland/deno/releases/download/v${DENO_VERSION}/${D}.zip" \
	&& unzip -j /tmp/deno.zip -d /out \
	&& chmod 755 /out/deno

# --- runtime: pinned yt-dlp + ffmpeg on a slim Debian ------------------------
FROM debian:trixie-slim AS runtime
ARG YT_DLP_VERSION
ENV PATH=/opt/venv/bin:$PATH
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates ffmpeg python3 python3-venv \
	&& rm -rf /var/lib/apt/lists/* \
	&& python3 -m venv /opt/venv \
	&& /opt/venv/bin/pip install --no-cache-dir "yt-dlp[default]==${YT_DLP_VERSION}" \
	&& useradd --system --create-home --home-dir /data --shell /usr/sbin/nologin app
COPY --from=build /out/server /usr/local/bin/server
COPY --from=deno /out/deno /usr/local/bin/deno
USER app
WORKDIR /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
	CMD ["server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/server"]
