GO ?= go
all: test build
build:
	${GO} build -o dserver ./cmd/dserver/main.go
	${GO} build -o dcat ./cmd/dcat/main.go
	${GO} build -o dgrep ./cmd/dgrep/main.go
	${GO} build -o dmap ./cmd/dmap/main.go
	${GO} build -o dtail ./cmd/dtail/main.go
install:
	${GO} install ./cmd/dserver/main.go
	${GO} install ./cmd/dcat/main.go
	${GO} install ./cmd/dgrep/main.go
	${GO} install ./cmd/dmap/main.go
	${GO} install ./cmd/dtail/main.go
clean:
	ls ./cmd/ | while read cmd; do \
	  test -f $$cmd && rm $$cmd; \
	done
vet:
	find . -type d | egrep -v '(./samples|./log|./doc)' | while read dir; do \
	  echo ${GO} vet $$dir; \
	  ${GO} vet $$dir; \
	done
lint:
	${GO} get golang.org/x/lint/golint
	find . -type d | while read dir; do \
	  echo golint $$dir; \
	  golint $$dir; \
	done
test:
	${GO} test ./... -v
