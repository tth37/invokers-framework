FROM alpine:latest
WORKDIR /root/
COPY build/gateway .
CMD ["./gateway"]
