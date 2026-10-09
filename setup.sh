#!/bin/sh
docker rm -f irctun 2>/dev/null
docker rmi -f irctun:latest 2>/dev/null
docker build -t irctun:latest .
docker run -d --name irctun -p 6667:6667 irctun:latest
