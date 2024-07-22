FROM alpine:latest
WORKDIR /root/
COPY build/invoker .
CMD ["./invoker"]
