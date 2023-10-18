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

FROM registry.access.redhat.com/ubi9/go-toolset@sha256:46f43b74fa5f32bfb5da3b9782c472adc54ada71eed7d4ef0c50f534b49cc311 AS builder
ENV APP_ROOT=/opt/app-root
ENV GOPATH=$APP_ROOT

WORKDIR $APP_ROOT/src/
ADD go.mod go.sum $APP_ROOT/src/
RUN go mod download

# Add source code
ADD ./ $APP_ROOT/src/

RUN go build -o server main.go
RUN CGO_ENABLED=1 go build -gcflags "all=-N -l" -o server_debug main.go

# debug compile options & debugger
FROM registry.access.redhat.com/ubi9/go-toolset@sha256:46f43b74fa5f32bfb5da3b9782c472adc54ada71eed7d4ef0c50f534b49cc311 as debug
RUN go install github.com/go-delve/delve/cmd/dlv@v1.8.0

# overwrite server and include debugger
COPY --from=builder /opt/app-root/src/server_debug /usr/local/bin/fulcio-server

# Multi-Stage production build
FROM registry.access.redhat.com/ubi9/ubi-minimal@sha256:d37a1ed7bd94ac08acac8ff8d388d1d2b4c9ba17d61f1573a6dab604e6ae4d4f as deploy

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