# Base image for go dependencies, to speed up builds when they haven't changed.
# For more, see https://github.com/neondatabase/go-chef
FROM golang:1.24.2-alpine@sha256:7772cb5322baa875edd74705556d08f0eeca7b9c4b5367754ce3f2f00041ccee AS chef

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
