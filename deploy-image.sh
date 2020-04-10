#!/bin/bash
set -eo pipefail

[ -z "$REGISTRY" ] && echo "Please set REGISTRY environment variable first" && exit 1

NAMESPACE=${NAMESPACE-block-devices-webhook}
REPOSITORY=${REPOSITORY-block-devices-webhook}

go build cmd/webhook/main.go
podman build -t $REGISTRY/$NAMESPACE/$REPOSITORY:latest .
podman push $REGISTRY/$NAMESPACE/$REPOSITORY:latest
