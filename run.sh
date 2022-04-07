#!/bin/sh -eu
PR_NUMBER=$1

started_at=$(date +%s)
eta() {
    local now
    now=$(date +%s)
    echo $((540 + $started_at - $now))
}

oc new-project obulatov-test
oc apply -f ./pipeline.yaml
cat ./pipeline-run.yaml | sed "s/{{PR_NUMBER}}/$PR_NUMBER/g" | oc apply -f -
oc apply -f ./manifests/quay-postgresql/ -f ./manifests/quay-redis/ -f ./manifests/route.yaml
while true; do
    status=$(oc get pipelinerun build -o jsonpath='{.status.conditions[?(@.type=="Succeeded")].status}')
    printf "[eta:%ss] %s\n" "$(eta)" "$(oc get pipelinerun build -o jsonpath='{.status.conditions[?(@.type=="Succeeded")]}')"
    if [ "$status" != "Unknown" ]; then
        break
    fi
    sleep 5
done
oc apply -f ./manifests/quay-app/
oc get route quay -o jsonpath='https://{.status.ingress[0].host}/{"\n"}'
