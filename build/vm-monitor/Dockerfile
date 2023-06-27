FROM rust:1.70-alpine as builder
WORKDIR /workspace

RUN apk add musl-dev git

# Ensures we reclone upon new commits
# https://stackoverflow.com/questions/35134713
#
# Note: unlike in the stackoverflow post we access the branches endpoint so that
# `latest_commit` will change upon a commit in any branch
ADD "https://api.github.com/repos/neondatabase/vm-monitor/branches" latest_commit

# FIXME RELEASE: don't clone testing
RUN git clone --branch fprasx/testing https://github.com/neondatabase/vm-monitor.git 
RUN cargo build --release --manifest-path vm-monitor/Cargo.toml

FROM builder
COPY --from=builder /workspace/vm-monitor/target/release/vm-monitor /usr/bin/vm-monitor
ENTRYPOINT [ "/usr/bin/vm-monitor" ]
