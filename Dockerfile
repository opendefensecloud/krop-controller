# Build the krop-controller binary (the single kcp controller at ./cmd/controller).
# Multi-arch: the builder runs on $BUILDPLATFORM and cross-compiles Go to $TARGETARCH;
# the distroless runtime stage has no RUN, so amd64+arm64 needs no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# golang image pins GOTOOLCHAIN=local; auto fetches the exact toolchain from go.mod.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
