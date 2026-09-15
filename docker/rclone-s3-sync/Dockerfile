FROM python:3.12-alpine

RUN apk add --no-cache rclone tzdata \
    && mkdir -p /state \
    && chown 503:20 /state

COPY sync.py /app/sync.py

CMD ["python3", "/app/sync.py"]
