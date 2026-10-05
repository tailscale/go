FROM node:22-alpine
WORKDIR /app
COPY common.js client.js server.js /app/
RUN printf '#!/bin/sh\nexec node /app/client.js "$@"\n' > /usr/local/bin/client && \
    printf '#!/bin/sh\nexec node /app/server.js "$@"\n' > /usr/local/bin/server && \
    chmod +x /usr/local/bin/client /usr/local/bin/server
ENTRYPOINT []
CMD ["server"]
