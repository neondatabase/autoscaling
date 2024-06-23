FROM golang:1.21-alpine as chef
RUN go install github.com/sharnoff/go-chef@ee64ced765864b0b55c9afc33435529681673553
WORKDIR /workspace

FROM chef as planner
COPY . .
RUN go-chef --prepare recipe.json

FROM chef as builder

# Build dependencies
COPY --from=planner /workspace/recipe.json recipe.json
RUN CGO_ENABLED=0 go-chef --cook recipe.json

COPY go.mod go.mod
COPY go.sum go.sum

COPY neonvm/apis             neonvm/apis
COPY neonvm/client           neonvm/client
COPY pkg/api                 pkg/api
COPY pkg/plugin              pkg/plugin
COPY pkg/util                pkg/util
COPY cmd/autoscale-scheduler cmd/autoscale-scheduler

ARG GIT_INFO

RUN CGO_ENABLED=0 go build \
    # -ldflags "-X ..." allows us to overwrite the value of a variable in a package \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/autoscale-scheduler/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
