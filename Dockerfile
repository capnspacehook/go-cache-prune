FROM golang:1.21.2-alpine AS builder

COPY . /build
WORKDIR /build

# add git so VCS info will be stamped in binary
# ignore warning that a specific version of git isn't pinned
# hadolint ignore=DL3018
RUN apk add --no-cache git

ARG CGO_ENABLED=0
ARG VERSION=devel
RUN go build -buildvcs=true -ldflags "-s -w -X main.version=${VERSION}" -trimpath -o go-cache-prune

FROM scratch
COPY --from=builder /build/go-cache-prune /go-cache-prune

ENTRYPOINT [ "/go-cache-prune" ]
