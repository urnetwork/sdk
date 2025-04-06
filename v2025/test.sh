#!/usr/bin/env bash

go get -t -u ./...
go test -v -race
