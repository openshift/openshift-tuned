FROM openshift/origin-release:golang-1.10 as builder
COPY . /go/src/github.com/openshift/openshift-tuned/
RUN cd /go/src/github.com/openshift/openshift-tuned && make build

FROM centos:7

ENV APP_ROOT=/var/lib/tuned
ENV PATH=${APP_ROOT}/bin:${PATH} HOME=${APP_ROOT}
WORKDIR ${APP_ROOT}
COPY --from=builder /go/src/github.com/openshift/openshift-tuned/assets ${APP_ROOT}
COPY --from=builder /go/src/github.com/openshift/openshift-tuned/tuned-wait ${APP_ROOT}/bin

RUN INSTALL_PKGS="tuned kernel-tools patch" && \
    yum -y --setopt=tsflags=nodocs install -y ${INSTALL_PKGS} && \
    rpm -V ${INSTALL_PKGS} && \
    (patch -p1 -d /usr/lib/python*/site-packages/tuned/daemon/ < patches/tuned.diff || :) && \ 
    sed -i 's/^\s*daemon.*$/daemon = 0/' /etc/tuned/tuned-main.conf && \
    touch /etc/sysctl.conf && \
    yum -y remove patch && \
    yum clean all && \
    rm -rf /var/cache/yum ~/patches

ENTRYPOINT [ "/var/lib/tuned/bin/run" ]
