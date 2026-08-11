# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are cached separately from the source so code edits don't
# re-download the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dr1wbot ./cmd/bot

# The named volume mounted at /data inherits this directory's ownership, which
# is what lets the nonroot user write the whitelist state file.
RUN mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/dr1wbot /dr1wbot
COPY --from=build --chown=nonroot:nonroot /data /data

ENV STATE_FILE=/data/state.json

USER nonroot:nonroot
ENTRYPOINT ["/dr1wbot"]
