#!/bin/bash

set -e

docker build -t cicdteam/vm-postgres:1 .
docker push -q cicdteam/vm-postgres:1
