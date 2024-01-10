#!/bin/bash

MULTUS_VERSION="v4.0.2-eksbuild.1"
MULTUS_URL="https://raw.githubusercontent.com/aws/amazon-vpc-cni-k8s/master/config/multus/${MULTUS_VERSION}/multus-daemonset-thick.yml"

curl -sfL ${MULTUS_URL} | sed 's/"logLevel": "verbose"/"logLevel": "error"/' >multus.yaml
