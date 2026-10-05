#!/bin/sh
# Downloads the Jetty jars listed in jars.sha256 from Maven Central
# into the current directory and verifies their checksums.
set -e
M=https://repo1.maven.org/maven2
for jar in $(awk '{print $2}' "$1"); do
	case "$jar" in
	slf4j-api-*)
		v=${jar#slf4j-api-}
		v=${v%.jar}
		url=$M/org/slf4j/slf4j-api/$v/$jar
		;;
	jetty-http2-*)
		a=${jar%-*}
		v=${jar##*-}
		v=${v%.jar}
		url=$M/org/eclipse/jetty/http2/$a/$v/$jar
		;;
	*)
		a=${jar%-*}
		v=${jar##*-}
		v=${v%.jar}
		url=$M/org/eclipse/jetty/$a/$v/$jar
		;;
	esac
	wget -q -O "$jar" "$url"
done
sha256sum -c "$1"
