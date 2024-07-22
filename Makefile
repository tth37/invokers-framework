invoker: cmd/invoker/invoker.go cmd/utils/kube.go pkg/invoker/invoker.go
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o build/invoker cmd/invoker/invoker.go

gateway: cmd/gateway/gateway.go cmd/utils/kube.go
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o build/gateway cmd/gateway/gateway.go

invoker-docker: invoker
	docker build -t docker-registry.tth37.xyz/invoker -f docker/invoker.Dockerfile .

gateway-docker: gateway
	docker build -t docker-registry.tth37.xyz/gateway -f docker/gateway.Dockerfile .

publish: invoker-docker gateway-docker
	docker push docker-registry.tth37.xyz/invoker
	docker push docker-registry.tth37.xyz/gateway

deploy:
	@helm uninstall invokers-framework -n invokers-framework || true
	@kubectl delete ns invokers-framework || true
	@helm install invokers-framework ./helm/invokers-framework --namespace invokers-framework --create-namespace

.PHONY: invoker-docker gateway-docker publish deploy