# Imagen chica y fija: la app es un binario de Go más git y ssh. No hace
# falta la imagen base de HA (s6, bashio). Alpine 3.22 trae git 2.49
# (merge-tree --write-tree necesita >= 2.38).
FROM alpine:3.22

RUN apk add --no-cache git openssh-client

ARG BUILD_ARCH
# El binario se compila afuera (scripts/build-addon.sh): el Supervisor solo
# construye con el contenido de esta carpeta.
COPY bin/hcs-${BUILD_ARCH} /usr/bin/hcs
COPY run.sh /run.sh
RUN chmod a+x /usr/bin/hcs /run.sh

CMD ["/run.sh"]
