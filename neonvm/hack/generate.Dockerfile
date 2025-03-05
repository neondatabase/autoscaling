FROM golang:1.23.7@sha256:1acb493b9f9dfdfe705042ce09e8ded908ce4fb342405ecf3ca61ce7f3b168c7

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
