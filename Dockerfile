# Build a static binary, run it on distroless (no shell, no libc).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /geoservices .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /geoservices /geoservices
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/geoservices"]
