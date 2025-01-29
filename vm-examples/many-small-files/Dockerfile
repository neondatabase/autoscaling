FROM rust:1.75-bullseye AS builder

WORKDIR /usr/src/app

# Copy only the dependency files first
COPY Cargo.toml Cargo.lock ./

COPY ./src ./src

# Build the actual binary
RUN cargo build --release

FROM neondatabase/compute-node-v16:latest
USER root

RUN apt update && apt install --no-install-recommends -y smem htop lsof sysstat

COPY --from=builder /usr/src/app/target/release/many-small-files /usr/local/bin/
COPY ./mem.py /root/mem.py

ENV RUST_LOG=info
USER postgres
ENTRYPOINT ["many-small-files"]
