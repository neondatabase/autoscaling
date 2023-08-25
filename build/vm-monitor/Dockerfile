FROM rust:1.70-alpine as builder
WORKDIR /workspace

RUN apk add musl-dev git

# Ensures we reclone upon new commits
# https://stackoverflow.com/questions/35134713
#
# Note: unlike in the stackoverflow post we access the branches endpoint so that
# `latest_commit` will change upon a commit in any branch
ADD "https://api.github.com/repos/neondatabase/neon/branches" latest_commit

RUN git clone https://github.com/neondatabase/neon.git
RUN cargo build --release --manifest-path neon/libs/vm_monitor/Cargo.toml
# Move binary so we can cargo clean
RUN mkdir -p /workspace/bin && cp /workspace/neon/target/release/vm-monitor /workspace/bin
# Cargo clean dramatically reduces the size of the image
RUN cargo clean --release --manifest-path neon/libs/vm_monitor/Cargo.toml

FROM builder
COPY --from=builder /workspace/bin/vm-monitor /usr/bin/vm-monitor
