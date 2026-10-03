#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "Python SDK smoke: installing the wheel in a clean virtualenv"
make smoke
