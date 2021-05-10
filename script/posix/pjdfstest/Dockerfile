FROM alpine:3.9 AS stage-build
WORKDIR /work
RUN apk add git automake autoconf build-base && \
    git clone https://github.com/pjd/pjdfstest.git /work/pjdfstest && \
    cd /work/pjdfstest && \
    git checkout a75ea1caf843a3a1e6c866b61bf954f68cee4a85 && \
    autoreconf -ifs && ./configure && make pjdfstest

FROM alpine:3.9 AS stage-final
WORKDIR /work
COPY --from=stage-build /work/pjdfstest /work/pjdfstest
RUN apk add openssl perl-test-harness-utils
CMD ["prove", "-rv", "/work/pjdfstest/tests"]