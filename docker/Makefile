all:
	cp ../dserver .
	docker build . -t dserver:develop
	rm ./dserver
run:
	# http://localhost:8080/debug/pprof/goroutines?debug=1
	docker run -p 2222:2222 -p 8080:8080 dserver:develop
spinup:
	./spinup.sh 10
spindown:
	./spindown.sh 10
