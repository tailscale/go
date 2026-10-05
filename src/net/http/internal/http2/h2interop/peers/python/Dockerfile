FROM python:3.13-alpine
RUN pip install --no-cache-dir h2==4.4.1 hpack==4.2.0 hyperframe==6.1.0
COPY common.py client.py server.py /opt/h2interop/
RUN printf '#!/bin/sh\nexec python3 -u /opt/h2interop/client.py "$@"\n' > /usr/local/bin/client && \
    printf '#!/bin/sh\nexec python3 -u /opt/h2interop/server.py "$@"\n' > /usr/local/bin/server && \
    chmod +x /usr/local/bin/client /usr/local/bin/server
