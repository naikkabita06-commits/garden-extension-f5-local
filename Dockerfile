FROM golang:1.25 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/gardener-extension-f5 ./cmd/gardener-extension-f5
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/svc-lb-bridge ./cmd/svc-lb-bridge
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/seed-service-lb-controller ./cmd/seed-service-lb-controller


FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /out/gardener-extension-f5 /gardener-extension-f5
COPY --from=builder /out/svc-lb-bridge /svc-lb-bridge
COPY --from=builder /out/seed-service-lb-controller /seed-service-lb-controller

USER 65532:65532

ENTRYPOINT ["/gardener-extension-f5"]