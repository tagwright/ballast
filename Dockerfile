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

# restic comes from Alpine's own package repo, not a separate binary
# download or a second build stage.
RUN apk add --no-cache restic ca-certificates tzdata

COPY --from=build /out/ballast /usr/local/bin/ballast

ENTRYPOINT ["ballast"]
CMD ["daemon"]
