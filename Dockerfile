# Build
FROM golang:1.26-alpine AS build
WORKDIR /src
# Dependencies first: they change far less often than the code, so the module
# layer is reused across builds.
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/
# CGO off + static: the final image is distroless static, with no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /manager ./cmd

# Runtime
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /manager /manager
# 65532 = nonroot in distroless. It matches the runAsUser the operator sets on
# the pods it creates, on purpose: one identity to remember.
USER 65532:65532
ENTRYPOINT ["/manager"]
