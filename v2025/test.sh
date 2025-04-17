#!/usr/bin/env bash

go clean -cache
go clean -modcache

go test -v -race
