#!/usr/bin/env zsh

go clean -cache
go clean -modcache

go test -v -race
