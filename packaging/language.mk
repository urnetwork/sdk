.DEFAULT_GOAL := package
.PHONY: all package native generate check-generated check-package smoke publish clean
GO ?= go
all: package
generate:
	$(GO) -C ../packaging run . generate
check-generated:
	$(GO) -C ../packaging run . check-generated
native: check-generated
	$(GO) -C ../packaging run . native $(LANGUAGE)
package: check-generated
	$(GO) -C ../packaging run . package $(LANGUAGE)
check-package:
	$(GO) -C ../packaging run . check $(LANGUAGE)
smoke: package
	$(MAKE) check-package
publish:
	$(GO) -C ../packaging run . publish $(REGISTRY)
clean:
	rm -rf dist
