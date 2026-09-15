#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "Kotlin SDK smoke: exercising the shared Java/JNA package consumer"
# The desktop Kotlin surface is the Java/JNA artifact. Its Makefile delegates
# to java, so this check compiles and runs the same native load path used by
# Kotlin callers without requiring a separate Kotlin compiler installation.
make package check-package
