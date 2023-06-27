FROM rust:1.70-alpine as builder
WORKDIR /workspace

RUN apk add musl-dev git

# Ensures we reclone upon new commits
# https://stackoverflow.com/questions/35134713
ADD "https://api.github.com/repos/neondatabase/vm-monitor/commits?per_page=1" latest_commit

RUN git clone https://github.com/neondatabase/vm-monitor.git 
RUN cargo build --release --manifest-path vm-monitor/Cargo.toml

FROM builder
COPY --from=builder /workspace/vm-monitor/target/release/vm-monitor /usr/bin/vm-monitor
ENTRYPOINT [ "/usr/bin/vm-monitor" ]
