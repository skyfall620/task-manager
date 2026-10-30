FROM golang:1 AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api

FROM scratch

COPY --from=build /out/api /api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/api"]
