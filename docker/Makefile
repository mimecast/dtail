all:
	cp ../dserver .
	docker build . -t dserver:develop
	rm ./dserver
run:
	docker run -p 2222:2222 dserver:develop
spinup:
	./spinup.sh 10
