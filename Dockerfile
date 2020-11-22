# This builds a container running DTail server
# The container can be used for developing and testing
# Purposes

FROM centos:8

RUN mkdir -p /etc/dserver /var/run/dserver

ADD ./samples/dtail.json.sample /etc/dserver/dtail.json
ADD ./dserver /usr/local/bin/dserver

RUN useradd dserver
RUN chown -R dserver /var/run/dserver
USER dserver

WORKDIR /var/run/dserver
EXPOSE 2222/tcp

CMD ["/usr/local/bin/dserver", "-cfg", "/etc/dserver/dtail.json"]
