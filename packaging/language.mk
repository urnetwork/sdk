.DEFAULT_GOAL := package
.PHONY: all package native generate check-package publish clean
GO ?= go
all: package
generate:
	$(GO) -C ../packaging run . generate
native: generate
	$(GO) -C ../packaging run . native $(LANGUAGE)
package: generate
	$(GO) -C ../packaging run . package $(LANGUAGE)
check-package:
	$(GO) -C ../packaging run . check $(LANGUAGE)
publish:
	$(GO) -C ../packaging run . publish $(REGISTRY)
clean:
	rm -rf dist
