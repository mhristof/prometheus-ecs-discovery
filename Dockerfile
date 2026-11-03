FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/prometheus-ecs-discovery .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bin/prometheus-ecs-discovery /bin/
ENTRYPOINT ["/bin/prometheus-ecs-discovery"]
