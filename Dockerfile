FROM openanolis/anolisos:8.9-x86_64

COPY bin/rp /usr/local/bin/rproxy

WORKDIR /root

CMD [ "/usr/local/bin/rproxy" ]
