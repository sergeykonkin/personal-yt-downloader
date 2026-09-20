.PHONY: help web-npm-install web-build embed go-build go-test web-test test build shortcut run-dev docker-build clean

help:
	@echo "Targets:"
	@echo "  build         Build the web UI, embed it, and compile the server to bin/server"
	@echo "  test          Run the Go test suite (race) and the web test suite"
	@echo "  shortcut      Lint and sign the iOS Shortcut into shortcuts/build/ (macOS)"
	@echo "  docker-build  Build the local dev image (personal-yt-downloader)"
	@echo "  run-dev       Build the dev image, then run it on :8080 (http, dev cookies)"
	@echo "  clean         Remove build outputs and the local data directory"

web-npm-install:
	cd web && npm install

web-build: web-npm-install
	cd web && npm run build

# Copy the built UI into the package the Go compiler embeds. It depends on
# the web build — without it a clean checkout would embed a stale or missing
# dist — and the embedded copy is gitignored.
embed: web-build
	rm -rf internal/app/web/dist
	mkdir -p internal/app/web
	cp -R web/dist internal/app/web/dist

go-build: embed
	go build -o bin/server ./cmd/server

go-test: embed
	go build ./... && go vet ./... && go test -race -count=1 ./...

web-test:
	cd web && npm test

test: go-test web-test

build: go-build

# Lint the Shortcut plist and sign it into an importable .shortcut. The
# native signer only accepts plist input through a .shortcut extension, so
# the source is copied under a temporary name and removed after signing.
# macOS only (Apple's shortcuts CLI); the signed build is a gitignored
# artifact — the plist is the committed source of truth.
shortcut:
	plutil -lint shortcuts/Download-YouTube-Video.plist
	mkdir -p shortcuts/build
	cp shortcuts/Download-YouTube-Video.plist shortcuts/build/Download-YouTube-Video-unsigned.shortcut
	shortcuts sign \
		--mode anyone \
		--input shortcuts/build/Download-YouTube-Video-unsigned.shortcut \
		--output "shortcuts/build/Download YouTube video.shortcut"
	rm shortcuts/build/Download-YouTube-Video-unsigned.shortcut

docker-build:
	docker build -t personal-yt-downloader .

# Local development run in a container: the image bundles yt-dlp, ffmpeg,
# and deno, so nothing beyond Docker is needed on the host. The login
# password is `local` unless PASSWORD is set in the environment; exporting
# API_TOKEN (optional) enables Bearer auth for direct API requests, unset
# or empty keeps it disabled. ./data holds the downloads, and cookies are
# marked insecure because plain http://localhost is not a secure context.
# Ctrl-C stops the server (graceful); --rm cleans up.
run-dev: docker-build
	docker run --rm -p 8080:8080 -e PASSWORD="$${PASSWORD:-local}" -e API_TOKEN="$${API_TOKEN:-}" \
		-v "$$(pwd)/data:/data" \
		personal-yt-downloader -insecure-cookies

clean:
	rm -rf bin web/dist shortcuts/build data
