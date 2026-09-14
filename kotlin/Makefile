.DEFAULT_GOAL := package
.PHONY: all package native generate check-package package-android clean
all: package
package native generate check-package publish package-android clean:
	$(MAKE) -C ../java $@
