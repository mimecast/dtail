# This builds a container running DTail server
# The container can be used for developing and testing
# Purposes

FROM fedora:34
RUN mkdir -p /etc/dserver /var/run/dserver/ /var/log/dserver

ADD ./dtail.json /etc/dserver/dtail.json
ADD ./dserver /usr/local/bin/dserver
ADD ./mapr_testdata.log /var/log/mapr_testdata.log

RUN useradd dserver
RUN chown -R dserver /var/run/dserver /var/log/dserver
USER dserver

WORKDIR /var/run/dserver
EXPOSE 2222/tcp

CMD ["/usr/local/bin/dserver", "-relaxedAuth", "-cfg", "/etc/dserver/dtail.json"]
