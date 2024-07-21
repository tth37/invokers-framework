# Invokers Framework

The Invokers Framework is a system designed to manage and interact with OpenFaaS functions across a Kubernetes cluster. It provides tools for building, deploying, and invoking functions on specific nodes.

## Prerequisites

Before you begin, ensure you have the following tools installed and configured:

- `faas-cli`: The OpenFaaS CLI tool
- `kubectl`: The Kubernetes command-line tool
- Access to a Kubernetes cluster
- Docker registry access (e.g., `docker-registry.tth37.xyz`)

## Setup and Usage

### Step 1: Build OpenFaaS Functions

Navigate to the directory containing your OpenFaaS function definitions and build them using `faas-cli`.

Example:

```bash
cd openfaas-fns
faas-cli build -f function1.yml
faas-cli push -f function1.yml # pushes to docker-registry.tth37.xyz/function1:latest
```

### Step 2: Deploy Invokers Framework

Deploy the Invokers Framework to your Kubernetes cluster:

```bash
make deploy
```

After deployment, note the External-IP of the Gateway. For example:

```
Gateway External-IP: 172.16.13.92
```

### Step 3: Use invokers_sdk

The `invokers_sdk` provides a Python client for interacting with the Invokers Framework. Here are some usage examples:

```python
from invokers_sdk import GatewayClient

# Initialize the client
client = GatewayClient("http://172.16.13.92:8080")

# Get information about nodes in the cluster
nodes = client.get_nodes()
print(nodes)

# Prewarm functions on specific nodes
client.prewarm({
    "intel-103": {
        "docker-registry.tth37.xyz/function1": True
    },
    "intel-104": {
        "docker-registry.tth37.xyz/function1": True
    },
    "intel-105": {
        "docker-registry.tth37.xyz/function1": True
    }
})

# Dispatch a request to multiple functions on different nodes
result = client.dispatch("Hello, world!", [
    {"node": "intel-103", "function": "docker-registry.tth37.xyz/function1"},
    {"node": "intel-104", "function": "docker-registry.tth37.xyz/function1"},
    {"node": "intel-105", "function": "docker-registry.tth37.xyz/function1"}
])
print(result)
```

## Additional Information

- Ensure that your Kubernetes cluster has the necessary permissions to pull images from your Docker registry.
- The `prewarm` method prepares the specified functions on the given nodes, which can reduce cold start times.
- The `dispatch` method allows you to send requests to multiple functions across different nodes in a single call.

For more detailed information about the API and advanced usage, please refer to the SDK documentation.
