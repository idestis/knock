# knock-relay (knockd). Build from the repo root: docker build -f relay/Dockerfile .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY protocol ./protocol
COPY relay ./relay
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/knockd ./relay/cmd/knockd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/knockd /knockd
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/knockd", "-addr", ":8787"]
