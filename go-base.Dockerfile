# Base image for go dependencies, to speed up builds when they haven't changed.
# For more, see https://github.com/neondatabase/go-chef
ARG GOLANG_IMG_TAG=replaceme
ARG GOLANG_IMG_SHA=replaceme
FROM golang:$GOLANG_IMG_TAG$GOLANG_IMG_SHA AS chef

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
