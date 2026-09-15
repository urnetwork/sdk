.DEFAULT_GOAL := package
.PHONY: all package check-package smoke clean
GO ?= go
all: package
package:
	$(GO) -C ../packaging run . mobile swift
check-package:
	cd dist/package && swift package dump-package > /dev/null
	$(GO) -C ../packaging run . release check-swift
smoke: package check-package
publish:
	$(GO) -C ../packaging run . publish swift
publish-cocoapods:
	$(GO) -C ../packaging run . publish cocoapods
clean:
	rm -rf dist
