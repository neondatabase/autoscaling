#!/usr/bin/env bash

pod="$(kubectl get pod -n kube-system -l name=autoscale-scheduler -o jsonpath='{.items[*].metadata.name}')"
kubectl logs -f -n kube-system "$pod"
