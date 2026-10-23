# The binary is built by goreleaser and staged in the build context as $TARGETPLATFORM/helm-debug,
# so this file only assembles the final image. The first stage exists only to fetch the CA bundle,
# which scratch lacks and without which TLS to the cluster API fails. It runs on the build platform
# because the bundle is the same for every architecture, so no QEMU emulation is needed for it.
FROM --platform=$BUILDPLATFORM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
ARG TARGETPLATFORM
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY $TARGETPLATFORM/helm-debug /helm-debug
# HOME=/ makes the default kubeconfig path /.kube/config, so `-v ~/.kube:/.kube:ro` just works.
ENV HOME=/
USER 65532:65532
ENTRYPOINT ["/helm-debug"]
