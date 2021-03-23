# This builds a container running DTail server
# The container can be used for developing and testing
# Purposes

FROM fedora:33
RUN mkdir -p /etc/dserver /var/run/dserver/ /var/log/dserver

ADD ./dtail.json /etc/dserver/dtail.json
ADD ./dserver /usr/local/bin/dserver

RUN useradd dserver
RUN chown -R dserver /var/run/dserver /var/log/dserver
USER dserver

WORKDIR /var/run/dserver
EXPOSE 2222/tcp
EXPOSE 8080/tcp

CMD ["/usr/local/bin/dserver", "-relaxedAuth", "-cfg", "/etc/dserver/dtail.json", "-pprof", "8080"]
