FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -o /blobstore .

FROM alpine:3.22
COPY --from=build /blobstore /blobstore
ENTRYPOINT ["/blobstore"]
