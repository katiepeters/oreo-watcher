#!/bin/sh
cd `dirname $0`

mkdir -p dist
go build -o dist/main .
tar -czvf dist/archive.tar.gz meta.json ./dist/main
