all: build

.PHONY: clean
clean:
	rm -r torb

deps:
	gb vendor restore

.PHONY: build
build:
	GOPATH=`pwd`:`pwd`/vendor go build -v torb
	sudo systemctl restart torb.go.service
