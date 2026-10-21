FROM golang:1.26.10-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./

RUN go mod download

COPY main.go ./
COPY internal ./internal

RUN CGO_ENABLED=1 go build -trimpath -o /image-service .

FROM debian:bookworm-slim

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

WORKDIR /app

COPY --from=build /image-service /app/image-service
COPY workflow.json /app/workflow.json

RUN mkdir /data && chown 65532:65532 /data

USER 65532:65532

ENV ADDR=0.0.0.0:8088 DB_PATH=/data/jobs.db WORKFLOW_PATH=/app/workflow.json

EXPOSE 8088

VOLUME ["/data"]

ENTRYPOINT ["/app/image-service"]
