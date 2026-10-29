# Build the krop-controller binary (the single kcp controller at ./cmd/controller).
# Multi-arch: the builder runs on $BUILDPLATFORM and cross-compiles Go to $TARGETARCH;
# the distroless runtime stage has no RUN, so amd64+arm64 needs no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
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

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
