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
# The console is a second binary in the same image, and a second DEPLOYMENT in
# the chart. One image because they share the API types and shipping two would
# mean two things to tag and two chances to run mismatched versions; two
# deployments because the manager's ServiceAccount can create and delete anybody
# is environments while the console's needs only `impersonate`. One process for
# both would give the web-facing half the identity of the half that can do
# everything.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /console ./cmd/console

# Runtime
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /manager /manager
COPY --from=build /console /console
# 65532 = nonroot in distroless. It matches the runAsUser the operator sets on
# the pods it creates, on purpose: one identity to remember.
USER 65532:65532
ENTRYPOINT ["/manager"]
