# This builds a container running DTail server
# The container can be used for developing and testing
# Purposes

FROM fedora:35
RUN mkdir -p /etc/dserver /var/run/dserver/cache /var/log/dserver

ADD ./dtail.json /etc/dserver/dtail.json
# NEXT: Compile dserver in a container as well, as otherwise might have glibc errors.
ADD ./dserver /usr/local/bin/dserver
ADD ./mapr_testdata.log /var/log/mapr_testdata.log

# Normal Linux user (simulates someone who want's to use DTail)
RUN useradd paul
ADD ./id_rsa_docker.pub /var/run/dserver/cache/paul.authorized_keys

# DTail server user
RUN useradd dserver
RUN chown -R dserver /var/run/dserver /var/log/dserver
USER dserver

WORKDIR /var/run/dserver
EXPOSE 2222/tcp

CMD ["/usr/local/bin/dserver", "-cfg", "/etc/dserver/dtail.json"]
