# Build natively on the runner and cross-compile for each target platform (no QEMU needed).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" -o /frigate-mcp .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /frigate-mcp /frigate-mcp
COPY LICENSE THIRD_PARTY_LICENSES /licenses/
EXPOSE 8091
ENTRYPOINT ["/frigate-mcp"]
