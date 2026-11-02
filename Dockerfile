# Multi-platform builds cross-compile: the build stages run on the builder's
# own platform and only the final stage is per platform, with no RUN steps,
# so no emulation is needed.

# Build the frontend (platform-independent output).
FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build the backend for the target platform, with the frontend embedded.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS api
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/*.go web/
COPY --from=web /src/web/dist web/dist
# timetzdata builds the time zone database into the binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -tags embedweb,timetzdata \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/hlm ./cmd/hlm

FROM alpine:3.22
ARG VERSION=dev
LABEL org.opencontainers.image.title="home-lab-manager" \
      org.opencontainers.image.source="https://github.com/danotchuuy/home-lab-manager" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=api /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=api /out/hlm /usr/local/bin/hlm
# hlm listens on :8080 and keeps its data in ./data, so /data from here. These
# aren't set with ENV: a GSH_DATA_DIR or GSH_LISTEN from before the short name
# changed from gsh to hlm would lose to an image default.
WORKDIR /
VOLUME /data
EXPOSE 8080
# Talking to the Docker socket requires root (or the socket's group).
ENTRYPOINT ["/usr/local/bin/hlm"]
