FROM alpine:latest
WORKDIR /root/
COPY invoker .
CMD ["./invoker"]
