FROM registry.svc.ci.openshift.org/openshift/release:golang-1.10 AS builder
WORKDIR /go/src/github.com/openshift/openshift-tuned
COPY . .
RUN make build

FROM registry.svc.ci.openshift.org/openshift/origin-v4.0:base
ENV APP_ROOT=/var/lib/tuned
ENV PATH=${APP_ROOT}/bin:${PATH} 
ENV HOME=${APP_ROOT}
WORKDIR ${APP_ROOT}
COPY --from=builder /go/src/github.com/openshift/openshift-tuned/openshift-tuned /usr/bin/
COPY --from=builder /go/src/github.com/openshift/openshift-tuned/assets ${APP_ROOT}
RUN INSTALL_PKGS=" \
      tuned hdparm kernel-tools patch \
      " && \
    yum install --setopt=tsflags=nodocs -y $INSTALL_PKGS && \
    rpm -V $INSTALL_PKGS && \
    (patch -p1 -d /usr/lib/python*/site-packages/tuned/daemon/ < patches/tuned.diff || :) && \ 
    sed -i 's/^\s*daemon.*$/daemon = 0/' /etc/tuned/tuned-main.conf && \
    touch /etc/sysctl.conf && \
    yum -y remove patch && \
    yum clean all && \
    rm -rf /var/cache/yum ~/patches
ENTRYPOINT [ "/var/lib/tuned/bin/run" ]
