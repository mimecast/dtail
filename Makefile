GO ?= go
all: build
build:
	${GO} version
	${GO} build
	cp -pv ./dtail ./dcat
	cp -pv ./dtail ./dgrep
	cp -pv ./dtail ./dmap
	cp -pv ./dtail ./dserver
clean:
	rm -v dtail dgrep dcat dmap dserver 2>/dev/null
install:
	${GO} install
	cp -pv ${GOPATH}/bin/dtail ${GOPATH}/bin/dcat
	cp -pv ${GOPATH}/bin/dtail ${GOPATH}/bin/dgrep
	cp -pv ${GOPATH}/bin/dtail ${GOPATH}/bin/dmap
	cp -pv ${GOPATH}/bin/dtail ${GOPATH}/bin/dserver
vet:
	find . -type d | while read dir; do \
	  echo ${GO} vet $$dir; \
	  ${GO} vet $$dir; \
	  done
lint:
	${GO} get golang.org/x/lint/golint
	find . -type d | while read dir; do \
	  echo ${GOPATH}/bin/golint $$dir; \
	  ${GOPATH}/bin/golint $$dir; \
	  done
