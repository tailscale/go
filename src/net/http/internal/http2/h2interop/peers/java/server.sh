#!/bin/sh
# The h2interop peer server using Jetty's HTTP/2 server.
# See JettyServer.java and ../README.md.
exec java -XX:+UseSerialGC -cp '/app/peer.jar:/app/lib/*' JettyServer "$@"
