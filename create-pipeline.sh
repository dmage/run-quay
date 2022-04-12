#!/bin/sh -eu
NAMESPACE=$1
kubectl -n "$NAMESPACE" create -f ./pipeline/
kubectl -n "$NAMESPACE" create configmap manifests --from-file=./manifests/
