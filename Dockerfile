# The in-cluster agent: the phrontis binary, static, on distroless, as the non-root user.
# Build on the runner's native arch and cross-compile: arm64 under QEMU took >30 min in CI.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# The default builds the public binary.
ARG CMD=./cmd/phrontis
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /phrontis $CMD

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /phrontis /phrontis
USER 65532:65532
ENTRYPOINT ["/phrontis"]
