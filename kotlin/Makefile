.DEFAULT_GOAL := package
.PHONY: all package native generate check-package smoke package-android clean
all: package
package native generate check-package smoke publish package-android clean:
	$(MAKE) -C ../java $@
