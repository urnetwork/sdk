LANGUAGE := java
REGISTRY := maven
include ../packaging/language.mk

.PHONY: package-android
# Consume the existing gomobile AAR; this is separate from the desktop JNA JAR.
package-android:
	$(GO) -C ../packaging run . mobile android
