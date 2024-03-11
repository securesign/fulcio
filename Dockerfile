#
# Copyright 2021 The Sigstore Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM registry.access.redhat.com/ubi9/go-toolset@sha256:9666cdca1df0003f60b5bf3862b58a910df5753ba7bd166a2117b044b29450cb AS builder
ENV APP_ROOT=/opt/app-root
ENV GOPATH=$APP_ROOT

WORKDIR $APP_ROOT/src/
ADD go.mod go.sum $APP_ROOT/src/
# Add source code
ADD ./ $APP_ROOT/src/

RUN go mod download && \
    go build -mod=readonly -o server main.go

# Multi-Stage production build
FROM registry.access.redhat.com/ubi9/go-toolset@sha256:9666cdca1df0003f60b5bf3862b58a910df5753ba7bd166a2117b044b29450cb as deploy

LABEL description="Fulcio is a free-to-use certificate authority for issuing code signing certificates for an OpenID Connect (OIDC) identity, such as email address."
LABEL io.k8s.description="Fulcio is a free-to-use certificate authority for issuing code signing certificates for an OpenID Connect (OIDC) identity, such as email address."
LABEL io.k8s.display-name="Fulcio container image for Red Hat Trusted Signer"
LABEL io.openshift.tags="fulcio trusted-signer"
LABEL summary="Provides the Fulcio CA for keyless signing with Red Hat Trusted Signer."
LABEL com.redhat.component="fulcio"

# Retrieve the binary from the previous stage
COPY --from=builder /opt/app-root/src/server /usr/local/bin/fulcio-server
# Set the binary as the entrypoint of the container
ENTRYPOINT ["/usr/local/bin/fulcio-server", "serve"]
#