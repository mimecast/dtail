GO ?= go
all: build
build:
	${GO} version
	${GO} build -o dtail ./cmd/dtail/main.go
	${GO} build -o dcat ./cmd/dcat/main.go
	${GO} build -o dgrep ./cmd/dgrep/main.go
	${GO} build -o dmap ./cmd/dmap/main.go
	${GO} build -o dserver ./cmd/dserver/main.go
clean:
	rm -v dtail dgrep dcat dmap dserver 2>/dev/null
install: build
	cp -pv dtail ${GOPATH}/bin/dtail
	cp -pv dcat ${GOPATH}/bin/dcat
	cp -pv dgrep ${GOPATH}/bin/dgrep
	cp -pv dmap ${GOPATH}/bin/dmap
	cp -pv dserver ${GOPATH}/bin/dserver
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
