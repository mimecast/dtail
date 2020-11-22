GO ?= go
all: test build
build:
	${GO} build -o dserver ./cmd/dserver/main.go
	${GO} build -o dcat ./cmd/dcat/main.go
	${GO} build -o dgrep ./cmd/dgrep/main.go
	${GO} build -o dmap ./cmd/dmap/main.go
	${GO} build -o dtail ./cmd/dtail/main.go
clean:
	ls ./cmd/ | while read cmd; do \
	  test -f $$cmd && rm $$cmd; \
	done
install: build
	cp -pv dserver ${GOPATH}/bin/dserver
	cp -pv dcat ${GOPATH}/bin/dcat
	cp -pv dgrep ${GOPATH}/bin/dgrep
	cp -pv dmap ${GOPATH}/bin/dmap
	cp -pv dtail ${GOPATH}/bin/dtail
vet:
	find . -type d | egrep -v '(./samples|./log|./doc)' | while read dir; do \
	  echo ${GO} vet $$dir; \
	  ${GO} vet $$dir; \
	done
lint:
	${GO} get golang.org/x/lint/golint
	find . -type d | while read dir; do \
	  echo ${GOPATH}/bin/golint $$dir; \
	  ${GOPATH}/bin/golint $$dir; \
	done
test:
	${GO} test ./... -v
