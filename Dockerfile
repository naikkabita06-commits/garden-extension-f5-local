# syntax=docker/dockerfile:1.7

FROM golang:1.25 AS builder

WORKDIR /src

# Corporate TLS interception can require an additional CA while the builder
# downloads Go modules. The secret is optional so clean CI/production builds do
# not depend on a workstation-only certificate, and it never enters the runtime
# image.
RUN --mount=type=secret,id=corporate_ca,required=false \
    if [ -f /run/secrets/corporate_ca ]; then \
      cp /run/secrets/corporate_ca /usr/local/share/ca-certificates/corporate-ca.crt && \
      update-ca-certificates; \
    fi

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -mod=readonly -trimpath -ldflags="-s -w" \
    -o /out/gardener-extension-f5 ./cmd/gardener-extension-f5


RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -mod=readonly -trimpath -ldflags="-s -w" \
    -o /out/svc-lb-bridge  ./cmd/svc-lb-bridge 

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -mod=readonly -trimpath -ldflags="-s -w" \
    -o /out/seed-service-lb-controller ./cmd/seed-service-lb-controller


FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /out/gardener-extension-f5 /gardener-extension-f5
COPY --from=builder /out/svc-lb-bridge /svc-lb-bridge
COPY --from=builder /out/seed-service-lb-controller /seed-service-lb-controller

USER 65532:65532

ENTRYPOINT ["/gardener-extension-f5"]
