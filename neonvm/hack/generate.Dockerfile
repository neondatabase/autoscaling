FROM golang:1.24.3@sha256:39d9e7d9c5d9c9e4baf0d8fff579f06d5032c0f4425cdec9e86732e8e4e374dc

RUN apt-get update && apt-get install -y patch

ARG CONTROLLER_TOOLS_VERSION
ARG CODE_GENERATOR_VERSION

# Use uid and gid of current user to avoid mismatched permissions
ARG USER_ID
ARG GROUP_ID
RUN if [ $USER_ID -ne $(id -u) ]; then \
        addgroup --gid $GROUP_ID user; \
        adduser --disabled-password --gecos '' --uid $USER_ID --gid $GROUP_ID user; \
    fi
USER $USER_ID:$GROUP_ID

WORKDIR /workspace

RUN git clone --branch=${CODE_GENERATOR_VERSION} --depth=1 https://github.com/kubernetes/code-generator.git $GOPATH/src/k8s.io/code-generator
RUN go install sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_TOOLS_VERSION}
