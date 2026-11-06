FROM python:3.11-alpine

# Install minimal runtime dependencies: OpenSSL (for AES-256-GCM), bash, curl, tzdata
RUN apk add --no-cache openssl bash curl tzdata

WORKDIR /app

# Copy the daemon script
COPY televault_daemon.py /app/televault_daemon.py

# Ensure executable
RUN chmod +x /app/televault_daemon.py

# Default ports: 8089 for REST API
EXPOSE 8089

ENTRYPOINT ["python3", "/app/televault_daemon.py"]
