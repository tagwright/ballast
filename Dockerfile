# Build from the parent directory so the sibling beacon module (local
# replace) is present:
#
#   docker build -f ballast/Dockerfile -t ghcr.io/tagwright/ballast:dev /mnt/md0/docker
#
# Once beacon is published and required as a tagged version (replace
# removed), this can build from the ballast directory alone.

FROM golang:1.25 AS build

WORKDIR /src
COPY beacon/ ./beacon/
COPY ballast/ ./ballast/
WORKDIR /src/ballast

RUN CGO_ENABLED=0 go build -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/ballast ./cmd/ballast

FROM alpine:3.20

# Alpine 3.20's restic package is stuck on 0.16.4. Pull a current,
# checksum-verified release straight from GitHub instead. This targets
# linux/amd64 only (the server this image runs on); arm64 support is a
# follow-up if it's ever needed.
ARG RESTIC_VERSION=0.19.1
ARG RESTIC_SHA256=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c

RUN apk add --no-cache ca-certificates tzdata bzip2 wget \
    && wget -O /tmp/restic.bz2 "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2" \
    && echo "${RESTIC_SHA256}  /tmp/restic.bz2" | sha256sum -c - \
    && bunzip2 /tmp/restic.bz2 \
    && mv /tmp/restic /usr/local/bin/restic \
    && chmod +x /usr/local/bin/restic \
    && restic version \
    && apk del bzip2 wget

COPY --from=build /out/ballast /usr/local/bin/ballast

ENTRYPOINT ["ballast"]
CMD ["daemon"]
