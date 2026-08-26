# Build from the ballast repository root:
#
#   docker build -t ghcr.io/tagwright/ballast:dev .
#
# beacon is consumed as a published module (github.com/tagwright/beacon).
# GOPRIVATE makes the build fetch tagwright's own modules directly from their
# source rather than through the public module proxy; go.sum still verifies
# their integrity.

FROM golang:1.25 AS build

ENV GOPRIVATE=github.com/tagwright/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/ballast ./cmd/ballast

FROM alpine:3.20

# Alpine 3.20's restic package is stuck on 0.16.4. Pull a current,
# checksum-verified release straight from GitHub instead. This targets
# linux/amd64 only (the server this image runs on); arm64 support is a
# follow-up if it's ever needed.
ARG RESTIC_VERSION=0.19.1
ARG RESTIC_SHA256=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c

RUN apk add --no-cache ca-certificates tzdata bzip2 wget openssh-client \
    && wget -O /tmp/restic.bz2 "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2" \
    && echo "${RESTIC_SHA256}  /tmp/restic.bz2" | sha256sum -c - \
    && bunzip2 /tmp/restic.bz2 \
    && mv /tmp/restic /usr/local/bin/restic \
    && chmod +x /usr/local/bin/restic \
    && restic version \
    && apk del bzip2 wget

# openssh-client above is not for Ballast itself (it never shells out to
# ssh directly): restic's sftp backend does, execing the "ssh" binary on
# PATH to open the SFTP session for any "sftp:user@host:path" destination.
# Without it, an sftp: destination is accepted as config (Destination.URL
# is opaque, engine-native syntax Ballast never parses) but every restic
# invocation against it fails at the "ssh: executable file not found in
# $PATH" step. See test/integration/run-sftp.sh.

COPY --from=build /out/ballast /usr/local/bin/ballast

ENTRYPOINT ["ballast"]
CMD ["daemon"]
