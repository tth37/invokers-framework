import requests
import json

class GatewayClient:
    def __init__(self, base_url):
        self.base_url = base_url.rstrip('/')

    def get_nodes(self):
        """
        Get the list of all nodes and their ClusterIPs.
        
        Returns:
            list: A list of dictionaries containing NodeName and ClusterIP.
        """
        response = requests.get(f"{self.base_url}/nodes")
        response.raise_for_status()
        return [node["NodeName"] for node in response.json()]

    def dispatch(self, input_data, steps):
        """
        Dispatch a task to the server.
        
        Args:
            input_data (str): The initial input for the task.
            steps (list): A list of dictionaries, each containing 'node' and 'function'.
        
        Returns:
            dict: The result of the task execution.
        """
        payload = {
            "input": input_data,
            "steps": steps
        }
        response = requests.post(f"{self.base_url}/dispatch", json=payload)
        response.raise_for_status()
        return response.json()
    
    def prewarm(self, nodes):
        """
        Prewarm the specified nodes.
        
        Args:
            nodes (dict): A dictionary of nodes and their images to prewarm.
        """
        payload = { "nodes": [] }
        for node, functions in nodes.items():
            for function, warm in functions.items():
                payload["nodes"].append({
                    "node": node,
                    "function": function,
                    "warm": warm
                })
        response = requests.post(f"{self.base_url}/prewarm", json=payload)
        response.raise_for_status()
        return