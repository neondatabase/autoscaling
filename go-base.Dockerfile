# Base image for go dependencies, to speed up builds when they haven't changed.
# For more, see https://github.com/neondatabase/go-chef
FROM golang:1.24.3-alpine@sha256:ef18ee7117463ac1055f5a370ed18b8750f01589f13ea0b48642f5792b234044 AS chef

ARG GO_CHEF_VERSION=v0.1.0
RUN go install github.com/neondatabase/go-chef@$GO_CHEF_VERSION
WORKDIR /workspace

FROM chef AS planner
COPY . .
# Produce a "recipe" containing information about all the packages imported.
# This step is usually NOT cached, but because the recipe is usually the same, follow-up steps
# usually WILL be cached.
RUN go-chef --prepare recipe.json

FROM chef AS builder
COPY --from=planner /workspace/recipe.json recipe.json
# Compile the dependencies baesd on the "recipe" alone -- usually cached.
RUN CGO_ENABLED=0 go-chef --cook recipe.json
