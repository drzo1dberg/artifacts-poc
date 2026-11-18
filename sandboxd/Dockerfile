FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /sandboxd .
WORKDIR /guest
COPY guest/go.mod guest/go.sum ./
RUN go mod download
COPY guest/*.go ./
RUN CGO_ENABLED=0 go build -o /sandbox-agent .

# Firecracker and a guest kernel from the Firecracker CI, both pinned by checksum.
FROM alpine:3.22 AS firecracker
ARG FC_VERSION=v1.17.0
ARG FC_SHA256=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
ARG KERNEL_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261028-84df49062576-0/x86_64/vmlinux-6.1.188
ARG KERNEL_SHA256=50048e55ab43115ef4798830cc8a8cde675f1d4d925b2450dcdb96ca5f77e0c1
RUN wget -qO fc.tgz https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz \
 && echo "${FC_SHA256}  fc.tgz" | sha256sum -c - \
 && tar xzf fc.tgz \
 && mv release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64 /firecracker \
 && wget -qO /vmlinux "${KERNEL_URL}" \
 && echo "${KERNEL_SHA256}  /vmlinux" | sha256sum -c -

# The disk "python": a normal container image plus the agent as init. sandboxd turns it into ext4.
FROM python:3.13-slim AS disk-python
COPY --from=build /sandbox-agent /sbin/sandbox-agent
RUN mkdir -p /workspace

FROM alpine:3.22
# GNU cp for --reflink, e2fsprogs for mkfs.ext4 -d, curl for trying the API with docker compose exec.
RUN apk add --no-cache coreutils curl e2fsprogs
COPY --from=firecracker /firecracker /usr/local/bin/firecracker
COPY --from=firecracker /vmlinux /opt/kernel/vmlinux
COPY --from=disk-python / /opt/disks/python/
COPY --from=build /sandboxd /usr/local/bin/sandboxd
ENTRYPOINT ["sandboxd"]
