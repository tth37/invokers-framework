invoker: cmd/invoker/invoker.go cmd/utils/kube.go pkg/invoker/invoker.go
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o invoker cmd/invoker/invoker.go

gateway: cmd/gateway/gateway.go cmd/utils/kube.go
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gateway cmd/gateway/gateway.go

invoker-docker: invoker
	docker build -t docker-registry.tth37.xyz/invoker -f docker/invoker.Dockerfile .

gateway-docker: gateway
	docker build -t docker-registry.tth37.xyz/gateway -f docker/gateway.Dockerfile .

publish: invoker-docker gateway-docker
	docker push docker-registry.tth37.xyz/invoker
	docker push docker-registry.tth37.xyz/gateway

deploy:
	kubectl delete ns invokers
	kubectl create ns invokers
	kubectl apply -f k8s/invokers.yaml
	@GATEWAY_IP=$$(kubectl get svc -n invokers -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}'); \
	echo "Gateway External-IP: $$GATEWAY_IP"; \