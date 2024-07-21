FROM alpine:latest
WORKDIR /root/
COPY gateway .
CMD ["./gateway"]
