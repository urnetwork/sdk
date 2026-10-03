LANGUAGE := csharp
REGISTRY := nuget
include ../packaging/language.mk

.PHONY: init check-tools
init:
	$(GO) -C ../packaging run . init $(LANGUAGE)
check-tools:
	$(GO) -C ../packaging run . check-tools $(LANGUAGE)
