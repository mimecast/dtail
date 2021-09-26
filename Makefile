GO ?= go
all: test build
build: dserver dcat dgrep dmap dtail
dserver:
ifndef USE_ACL
	${GO} build ${GO_FLAGS} -o dserver ./cmd/dserver/main.go
else
	${GO} build ${GO_FLAGS} -tags linuxacl -o dserver ./cmd/dserver/main.go
endif
dcat:
	${GO} build ${GO_FLAGS} -o dcat ./cmd/dcat/main.go
dgrep:
	${GO} build ${GO_FLAGS} -o dgrep ./cmd/dgrep/main.go
dmap:
	${GO} build ${GO_FLAGS} -o dmap ./cmd/dmap/main.go
dtail:
	${GO} build ${GO_FLAGS} -o dtail ./cmd/dtail/main.go
install:
ifndef USE_ACL
	${GO} install ./cmd/dserver/main.go
else
	${GO} install -tags linuxacl ./cmd/dserver/main.go
endif
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
	grep -R TODO: .
lint:
	${GO} get golang.org/x/lint/golint
	find . -type d | while read dir; do \
	  echo golint $$dir; \
	  golint $$dir; \
	done
test:
ifndef USE_ACL
	${GO} test -race ./... -v
else
	${GO} test -race -tags linuxacl ./... -v
endif
