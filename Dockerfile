# syntax=docker/dockerfile:1
# Pin these to digests (image@sha256:...) once you've pulled them; Dependabot
# will then keep the digests current.
ARG GO_IMAGE=golang:1.24-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG CMD
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# distroless: no shell, no package manager, runs as uid 65532.
FROM ${RUNTIME_IMAGE}
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
