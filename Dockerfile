# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The developer image: compiles cellad inside the image so `docker build .`
# from a checkout is enough. Dockerfile.ci is the release image: it copies a
# binary the pipeline already built, checksummed, signed and attested, and
# its runtime stage is the one between the two markers below, byte for byte,
# which TestRuntimeStagesMatch reads. A released image therefore differs from
# a developer's in where the binary came from and in nothing else.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X latere.ai/x/cella/internal/version.Version=${VERSION} -X latere.ai/x/cella/internal/version.Commit=${COMMIT} -X latere.ai/x/cella/internal/version.Date=${DATE}" \
      -o /out/cellad ./cmd/cellad

# >>> shared runtime base <<<
# cellad forks no binary of its own, so the runtime stage is distroless:
# CA roots for the OIDC issuers and webhooks it dials, nothing else. It runs
# as the image's non-root user, with both listeners' ports exposed and the
# data directory a volume.
FROM gcr.io/distroless/static-debian12:nonroot
VOLUME ["/var/lib/cella"]
EXPOSE 8080 8081
USER nonroot:nonroot
# <<< shared runtime base >>>
COPY --from=build /out/cellad /usr/local/bin/cellad
ENTRYPOINT ["/usr/local/bin/cellad"]
