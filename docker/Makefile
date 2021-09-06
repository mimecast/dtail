all: build
testrun: build spinup dcat spindown
build:
	cp ../dserver .
	docker build . -t dserver:develop
	rm ./dserver
spinup:
	./spinup.sh 10
spindown:
	./spindown.sh 10
dtail:
	../dtail --servers serverlist.txt --files '/var/log/dserver/*,/does/not/exist' --trustAllHosts --debug
dcat:
	../dcat --servers serverlist.txt --files '/etc/passwd' --trustAllHosts --debug
spinup1:
	docker run -p 2222:2222 dserver:develop
