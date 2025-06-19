#!/usr/bin/env zsh

go test -timeout 0 -v -race "$@"
