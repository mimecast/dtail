GO ?= go
all: build
build: dserver dcat dgrep dmap dtail dtailhealth
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
dtailhealth:
	${GO} build ${GO_FLAGS} -o dtailhealth ./cmd/dtailhealth/main.go
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
	${GO} install ./cmd/dtailhealth/main.go
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
	done | grep -F .go:
test:
	${GO} clean -testcache
ifndef USE_ACL
	set -e; find . -name '*_test.go' | while read file; do dirname $$file; done | \
		sort -u | while read dir; do ${GO} test --race -v $$dir || exit 2; done
else
	set -e;find . -name '*_test.go' | while read file; do dirname $$file; done | \
		sort -u | while read dir; do ${GO} test --tags linuxacl --race -v $$dir || exit 2; done
endif
