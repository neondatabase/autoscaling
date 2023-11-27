#!/bin/bash

MULTUS_VERSION="v4.0.2"
MULTUS_URL="https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/${MULTUS_VERSION}/deployments/multus-daemonset-thick.yml"

curl -sfL ${MULTUS_URL} | sed 's/"logLevel": "verbose"/"logLevel": "error"/' >multus.yaml
