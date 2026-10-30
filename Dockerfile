# Build stages run on the build machine's platform and cross-compile, so
# multi-arch images build without emulation; only the final stage is
# per-architecture.
FROM --platform=$BUILDPLATFORM node:26-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS backend
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /kvui ./cmd/kvui

FROM gcr.io/distroless/static:nonroot
COPY --from=backend /kvui /app/kvui
COPY --from=frontend /src/dist /app/static
USER 65532:65532
ENTRYPOINT ["/app/kvui"]
