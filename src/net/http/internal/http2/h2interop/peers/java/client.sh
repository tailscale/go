#!/bin/sh
# The h2interop peer client using the JDK's java.net.http.HttpClient.
# See Client.java and ../README.md.
# CDS may be set to override the class data sharing flags.
exec java \
	-XX:+UseSerialGC -XX:TieredStopAtLevel=1 -Xshare:auto \
	${CDS:--XX:SharedArchiveFile=/app/client.jsa} \
	-Djdk.internal.httpclient.disableHostnameVerification=true \
	-Djdk.httpclient.allowRestrictedHeaders=host,connection,upgrade,expect \
	-cp /app/peer.jar Client "$@"
