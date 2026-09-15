#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "Java SDK smoke: loading the packaged JNA artifact in a Maven consumer"
make package check-package
