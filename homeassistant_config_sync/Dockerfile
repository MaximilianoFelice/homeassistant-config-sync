# Small, pinned image: the app is a Go binary plus git and ssh. The HA base
# image (s6, bashio) is not needed. Alpine 3.22 ships git 2.49
# (merge-tree --write-tree needs >= 2.38).
FROM alpine:3.22

RUN apk add --no-cache git openssh-client

ARG BUILD_ARCH
# The binary is built outside (scripts/build-addon.sh): the Supervisor only
# builds with the contents of this folder.
COPY bin/hcs-${BUILD_ARCH} /usr/bin/hcs
COPY run.sh /run.sh
RUN chmod a+x /usr/bin/hcs /run.sh

CMD ["/run.sh"]
