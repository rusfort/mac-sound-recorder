#!/bin/bash

brew install lame
cd callrec
go build -o callrec .
./callrec 