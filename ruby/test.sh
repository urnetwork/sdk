#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "Ruby SDK smoke: installing the gem in an isolated GEM_HOME"
make package check-package
