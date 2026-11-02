.PHONY: build web api test run dev-api dev-web docker docker-push release clean

# The version stamped into builds: the current git tag, or a description of
# the commit (e.g. v1.2.0-3-gabc1234) between releases.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Where docker-push publishes the image (Docker Hub by default), and for
# which platforms.
IMAGE ?= danotchuuy/home-lab-manager
PLATFORMS ?= linux/amd64,linux/arm64

build: web api

web:
	cd web && npm ci && npm run build

# The binary embeds the frontend, so bin/hlm is all an install needs.
api:
	CGO_ENABLED=0 go build -tags embedweb -ldflags "$(LDFLAGS)" -o bin/hlm ./cmd/hlm

test:
	go vet ./...
	go test ./...
	cd web && npm run typecheck

run: build
	./bin/hlm

# For development, run these two in separate terminals and open
# http://<this-machine>:5173 from any computer on the network. Vite proxies
# /api to the backend on :8080. Both listen on all interfaces; set
# HLM_DEV_HOST=localhost (and HLM_LISTEN=127.0.0.1:8080) to keep them local.
dev-api:
	HLM_DEBUG=1 HLM_LISTEN=$${HLM_LISTEN:-0.0.0.0:8080} go run ./cmd/hlm

dev-web:
	cd web && npm run dev

docker:
	docker build --build-arg VERSION=$(VERSION) -t home-lab-manager .

# Build the image for every platform in PLATFORMS and push it to IMAGE,
# tagged with VERSION. A release version (vX.Y.Z, from a clean checkout of a
# tag) is also tagged "latest", which installs following that tag update to;
# other builds never are. Log in first, e.g. for Docker Hub with an access
# token: echo $$TOKEN | docker login -u danotchuuy --password-stdin
# Uses its own buildx builder, "hlm-builder", created on first use.
docker-push:
	@docker buildx inspect hlm-builder >/dev/null 2>&1 || \
		docker buildx create --name hlm-builder --driver docker-container --driver-opt network=host >/dev/null
	@release=; echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' && release=1; \
	tags="--tag $(IMAGE):$(VERSION)"; \
	if [ -n "$$release" ]; then tags="$$tags --tag $(IMAGE):latest"; fi; \
	if [ -z "$$release" ]; then echo "$(VERSION) isn't a release version, so :latest is left alone."; fi; \
	echo "Pushing $(VERSION) to $(IMAGE) for $(PLATFORMS)"; \
	docker buildx build --builder hlm-builder --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) $$tags --push .

# Build the release binaries and checksums the self-updater downloads. The
# release workflow does this when a v* tag is pushed. The gsh_* copies are for
# installs from before the short name changed from gsh to hlm, whose updater
# looks for those.
release: web
	rm -rf dist && mkdir -p dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -tags embedweb \
			-ldflags "$(LDFLAGS)" -o dist/hlm_linux_$$arch ./cmd/hlm || exit 1; \
		cp dist/hlm_linux_$$arch dist/gsh_linux_$$arch; \
	done
	cd dist && sha256sum hlm_* gsh_* > checksums.txt

clean:
	rm -rf bin dist web/dist
