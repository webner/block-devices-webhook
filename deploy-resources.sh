#!/bin/bash

function create_certs() {
    openssl req -x509 -newkey rsa:4096 -keyout tls.key -out tls.crt -days 36500 -nodes -subj "/CN=block-devices-webhook.block-devices-webhook.svc"
}

[ ! -e "tls.key" ] && oc extract secrets/webhook-cert
[ ! -e "tls.key" ] && create_certs

export CERT=$(cat tls.crt | openssl base64 -A)
export KEY=$(cat tls.key | openssl base64 -A)

for f in openshift/*.yaml; do
  cat $f | envsubst | oc apply -f -
done

