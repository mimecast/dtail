all: build
testrun: build spinup dcat spindown
serverfarm: spindown build spinup
build:
	cp ../testdata/mapr_testdata.log .
	cp ../dserver .
	docker build . -t dserver:develop
	rm ./dserver
spinup:
	./spinup.sh 10
spindown:
	./spindown.sh 10
dtail:
	../dtail --servers serverlist.txt --files '/var/log/dserver/*' --trustAllHosts --logLevel DEBUG
dtail2:
	../dtail --servers serverlist.txt --files '/var/log/dserver/*' --trustAllHosts --logLevel DEBUG --query 'from stats select max($$goroutines),count($$hostname),$$hostname,last($$time) group by $$hostname order by max($$goroutines)'
dgrep:
	../dgrep --servers serverlist.txt --files '/var/log/dserver/*' --regex MAPREDUCE --trustAllHosts
dcat:
	../dcat --servers serverlist.txt --files '/etc/passwd' --trustAllHosts
dcat_notrust:
	../dcat --servers serverlist.txt --files '/etc/passwd'
dcat2:
	# TODO: All serverless tests in this Makefile have to move to actual unit tests
	../dcat /etc/passwd
dmap:
	../dmap --servers serverlist.txt --files '/var/log/dserver/*' --trustAllHosts --query 'from stats select avg($$goroutines),max($$goroutines),min($$goroutines),last($$goroutines),count($$hostname),$$hostname group by $$hostname order by avg($$goroutines)'
dmap2:
	../dmap --servers serverlist.txt --files '/var/log/mapr_testdata.log' --trustAllHosts --query 'from stats select count($$time),last($$time) group by $$time order by count($$time) outfile dmap2-A.csv'
	../dmap --servers serverlist.txt --files '/var/log/mapr_testdata.log' --trustAllHosts --query 'from stats select count($$time),last($$time) group by $$time order by count($$time) outfile dmap2-B.csv'
	@echo Expecting zero diff!
	diff -u <(sort dmap2-A.csv) <(sort dmap2-B.csv)
dmap3:
	../dmap --servers <(head -n 1 serverlist.txt) --files '/var/log/mapr_testdata.log' --trustAllHosts --query 'from stats select count($$time),last($$time) group by $$time order by count($$time) outfile dmap2-A.csv'
	../dmap --query 'from stats select count($$time),last($$time) group by $$time order by count($$time) outfile dmap2-serverless.csv' ./mapr_testdata.log
	@echo Expecting zero diff!
	diff -u <(sort dmap2-A.csv) <(sort dmap2-serverless.csv)
spinup1:
	docker run -p 2222:2222 dserver:develop
