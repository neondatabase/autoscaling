#!/bin/bash

set -xe

docker build -t sergeyneon/vm-postgres:6 .
docker push sergeyneon/vm-postgres:6
