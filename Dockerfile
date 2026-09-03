# Build the krop-controller binary (the single kcp controller at ./cmd/controller).
# Multi-arch: the builder runs on $BUILDPLATFORM and cross-compiles Go to $TARGETARCH;
# the distroless runtime stage has no RUN, so amd64+arm64 needs no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS build
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

FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
