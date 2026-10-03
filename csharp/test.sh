#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "C# SDK smoke: loading the NuGet package in a clean dotnet consumer"
make smoke
