# The hopai runner image the platform starts as a Job (plan, rehearsal,
# recommend, onboard). Build the two binaries first, then the image:
#
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/hopai ./cmd/hopai
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/sandboxctl ./cmd/sandboxctl
#   docker build --platform linux/amd64 -t hopcharlie/hopai:<sha7> .
#
# git is needed by `hopai onboard` and `hopai prepare` (clone, commit, push of
# the contract branch). git-lfs is not installed on purpose: hopai turns LFS
# filters off and refuses LFS content.
FROM alpine:3.22.8

RUN apk add --no-cache ca-certificates git \
 && adduser -D -u 10001 hopai

COPY build/hopai build/sandboxctl /usr/local/bin/

USER 10001
ENTRYPOINT ["/usr/local/bin/hopai"]
