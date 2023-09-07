GO ?= go
ifdef DTAIL_USE_ACL
GO_TAGS=linuxacl
endif
ifdef DTAIL_USE_PROPRIETARY
GO_TAGS+=proprietary
endif
all: build
build: dserver dcat dgrep dmap dtail dtailhealth
dserver:
	${GO} build ${GO_FLAGS} -tags '${GO_TAGS}' -o dserver ./cmd/dserver/main.go
dcat:
	${GO} build ${GO_FLAGS} -tags '${GO_TAGS}' -o dcat ./cmd/dcat/main.go
dgrep:
	${GO} build ${GO_FLAGS} -tags '${GO_TAGS}' -o dgrep ./cmd/dgrep/main.go
dmap:
	${GO} build ${GO_FLAGS} -tags '${GO_TAGS}' -o dmap ./cmd/dmap/main.go
dtail:
	${GO} build ${GO_FLAGS} -tags '${GO_TAGS}' -o dtail ./cmd/dtail/main.go
dtailhealth:
	${GO} build ${GO_FLAGS} -tags '${GO_TAGS}' -o dtailhealth ./cmd/dtailhealth/main.go
install:
	${GO} install -tags '${GO_TAGS}' ./cmd/dserver/main.go
	${GO} install -tags '${GO_TAGS}' ./cmd/dcat/main.go
	${GO} install -tags '${GO_TAGS}' ./cmd/dgrep/main.go
	${GO} install -tags '${GO_TAGS}' ./cmd/dmap/main.go
	${GO} install -tags '${GO_TAGS}' ./cmd/dtail/main.go
	${GO} install -tags '${GO_TAGS}' ./cmd/dtailhealth/main.go
clean:
	ls ./cmd/ | while read cmd; do \
	  test -f $$cmd && rm $$cmd; \
	done
vet:
	find . -type d | egrep -v '(./examples|./log|./doc)' | while read dir; do \
	  echo ${GO} vet $$dir; \
	  ${GO} vet $$dir; \
	done
	sh -c 'grep -R NEXT: .'
	sh -c 'grep -R TODO: .'
lint:
	${GO} get golang.org/x/lint/golint
	find . -type d | while read dir; do \
	  echo golint $$dir; \
	  golint $$dir; \
	done | grep -F .go:
test:
	${GO} clean -testcache
	set -e; find . -name '*_test.go' | while read file; do dirname $$file; done | \
		sort -u | while read dir; do ${GO} test -tags '${GO_TAGS}' --race -v $$dir || exit 2; done
