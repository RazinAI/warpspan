# Agent image. Build from the repo root:
#   docker build -f agent/Dockerfile -t warpspan-agent .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY agent ./agent
COPY cmd/warpspan ./cmd/warpspan
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/warpspan ./cmd/warpspan

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/warpspan /warpspan
ENTRYPOINT ["/warpspan"]
