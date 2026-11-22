# The recorder mock of the integration proof. main.go uses the standard
# library only, so it builds as a single file with no module and no network.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /recorder main.go

FROM alpine:3
COPY --from=build /recorder /recorder
USER 65534
EXPOSE 8080 8081
ENTRYPOINT ["/recorder"]
