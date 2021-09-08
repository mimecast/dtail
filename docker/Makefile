all: build
testrun: build spinup dcat spindown
serverfarm: spindown build spinup
build:
	cp ../dserver .
	docker build . -t dserver:develop
	rm ./dserver
spinup:
	./spinup.sh 10
spindown:
	./spindown.sh 10
dtail:
	../dtail --servers serverlist.txt --files '/var/log/dserver/*' --trustAllHosts --debug
dgrep:
	../dgrep --servers serverlist.txt --files '/var/log/dserver/*' --regex MAPREDUCE --trustAllHosts
dcat:
	../dcat --servers serverlist.txt --files '/etc/passwd' --trustAllHosts --debug
dmap:
	../dmap --servers serverlist.txt --files '/var/log/dserver/*' --trustAllHosts --query 'from stats select avg(goroutines),max(goroutines),min(goroutines),last(goroutines),count($$hostname),$$hostname group by $$hostname order by avg(goroutines)'
spinup1:
	docker run -p 2222:2222 dserver:develop
