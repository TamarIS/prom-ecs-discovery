FROM golang:1.15-alpine
WORKDIR /src
RUN apk --no-cache add git
COPY *.go go.mod go.sum ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/prom-ecs-discovery .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=0 /bin/prom-ecs-discovery /bin/
ENTRYPOINT ["prom-ecs-discovery"]
