FROM eclipse-temurin:21-jdk-alpine

# Jetty, for the server. The client uses only the JDK.
WORKDIR /app/lib
COPY fetch-jars.sh jars.sha256 /src/
RUN sh /src/fetch-jars.sh /src/jars.sha256

WORKDIR /src
COPY Json.java Client.java JettyServer.java /src/
RUN javac -cp '/app/lib/*' -d /src/classes Json.java Client.java JettyServer.java && \
	jar cf /app/peer.jar -C /src/classes .
COPY client.sh /usr/local/bin/client
COPY server.sh /usr/local/bin/server
RUN chmod 755 /usr/local/bin/client /usr/local/bin/server

# Create a class data sharing archive to speed up client startup.
# (CDS requires classes in a jar, not a directory.) The request fails,
# since nothing listens on port 1, but it loads most of the HttpClient
# classes.
RUN echo '{"url":"https://127.0.0.1:1","requests":[{"method":"POST","path":"/","body_len":10}]}' | \
	CDS=-XX:ArchiveClassesAtExit=/app/client.jsa client
