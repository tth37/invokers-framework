package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type NodeClusterIP struct {
	NodeName  string
	ClusterIP string
}

type DispatchStep struct {
	Node     string `json:"node"`
	Function string `json:"function"`
}

type DispatchRequest struct {
	Input string         `json:"input"`
	Steps []DispatchStep `json:"steps"`
}

type PrewarmNodeRequest struct {
	Node     string `json:"node"`
	Function string `json:"function"`
	Warm     bool   `json:"warm"`
}

type PrewarmRequest struct {
	Nodes []PrewarmNodeRequest `json:"nodes"`
}

// Request represents the incoming request structure
type Request struct {
	Input    string `json:"input"`
	Function string `json:"function"`
	Steps    []Step `json:"steps"`
}

// Response represents the outgoing response structure
type Response struct {
	Output  string   `json:"output"`
	Results []Result `json:"results"`
}

// Step represents each step in the process
type Step struct {
	IP       string `json:"ip"`
	Function string `json:"function"`
}

// Result represents the output of each step
type Result struct {
	Output            string        `json:"output"`
	ColdStartDuration time.Duration `json:"coldStartDuration"`
	RunningDuration   time.Duration `json:"runningDuration"`
	StartAt           time.Time     `json:"startAt"`
	EndAt             time.Time     `json:"endAt"`
}

var nodeClusterIPs []NodeClusterIP
var clientset *kubernetes.Clientset

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	daemonSetName := os.Getenv("DAEMONSET_NAME")
	namespace := os.Getenv("NAMESPACE")

	if daemonSetName == "" || namespace == "" {
		log.Fatalf("DAEMONSET_NAME and NAMESPACE environment variables must be set")
	}

	pods, err := getPodsList(clientset, namespace, daemonSetName)
	if err != nil {
		log.Fatalf("Failed to get pods: %v", err)
	}

	nodeClusterIPs, err = createServicesForPods(clientset, namespace, pods)
	if err != nil {
		log.Fatalf("Failed to create services: %v", err)
	}

	http.HandleFunc("/nodes", handleNodes)
	http.HandleFunc("/dispatch", handleDispatch)
	http.HandleFunc("/prewarm", handlePrewarm)

	log.Println("Starting server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	json.NewEncoder(w).Encode(nodeClusterIPs)
}

func handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	var invReq Request
	invReq.Input = req.Input
	invReq.Function = req.Steps[0].Function
	invReq.Steps = make([]Step, len(req.Steps)-1)
	for i, step := range req.Steps[1:] {
		invReq.Steps[i] = Step{
			IP:       getClusterIPForNode(step.Node),
			Function: step.Function,
		}
	}

	resp, err := sendRequest(getClusterIPForNode(req.Steps[0].Node), invReq)
	if err != nil {
		http.Error(w, "Failed to send request", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handlePrewarm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PrewarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Concurrently send prewarm requests
	// A counter is used to wait for all requests to finish

	// for _, nodeReq := range req.Nodes {
	// 	go func(nodeReq PrewarmNodeRequest) {
	// 		log.Printf("Sending prewarm request to node %s\n", nodeReq.Node)
	// 		if err := sendPrewarmRequest(getClusterIPForNode(nodeReq.Node), nodeReq); err != nil {
	// 			http.Error(w, "Failed to send prewarm request", http.StatusInternalServerError)
	// 			return
	// 		}
	// 		done <- struct{}{}
	// 	}(nodeReq)
	// }

	var wg sync.WaitGroup
	successChan := make(chan bool, len(req.Nodes))

	for _, nodeReq := range req.Nodes {
		wg.Add(1)
		go func(nodeReq PrewarmNodeRequest) {
			defer wg.Done()
			log.Printf("Sending prewarm request to node %s\n", nodeReq.Node)
			if err := sendPrewarmRequest(getClusterIPForNode(nodeReq.Node), nodeReq); err != nil {
				log.Printf("Failed to send prewarm request to node %s: %v\n", nodeReq.Node, err)
				successChan <- false
				return
			}
			successChan <- true
		}(nodeReq)
	}

	go func() {
		wg.Wait()
		close(successChan)
	}()

	allSuccessful := true
	for success := range successChan {
		if !success {
			allSuccessful = false
			break
		}
	}

	if !allSuccessful {
		http.Error(w, "Failed to send prewarm requests", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func sendRequest(ip string, req Request) (Response, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}

	resp, err := http.Post(fmt.Sprintf("http://%s/invoke", ip), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}

	var response Response
	if err := json.Unmarshal(body, &response); err != nil {
		return Response{}, err
	}

	return response, nil
}

func sendPrewarmRequest(ip string, nodeRequest PrewarmNodeRequest) error {
	// If warm is true, then send <ip>/prewarm, else send <ip>/ensure

	var url string
	if nodeRequest.Warm {
		url = fmt.Sprintf("http://%s/prewarm", ip)
	} else {
		url = fmt.Sprintf("http://%s/ensure", ip)
	}

	// plain/text data, nodeRequest.Function
	resp, err := http.Post(url, "text/plain", bytes.NewBuffer([]byte(nodeRequest.Function)))
	if err != nil {
		return err
	}

	log.Printf("Sent prewarm request to %s, Response code: %d\n", ip, resp.StatusCode)

	defer resp.Body.Close()
	return nil
}

func getClusterIPForNode(nodeName string) string {
	for _, nc := range nodeClusterIPs {
		if nc.NodeName == nodeName {
			return nc.ClusterIP
		}
	}
	return ""
}

func getPodsList(clientset *kubernetes.Clientset, namespace, daemonSetName string) (*corev1.PodList, error) {
	return clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", daemonSetName),
	})
}

func createServicesForPods(clientset *kubernetes.Clientset, namespace string, pods *corev1.PodList) ([]NodeClusterIP, error) {
	var nodeClusterIPs []NodeClusterIP

	for _, pod := range pods.Items {
		serviceName := fmt.Sprintf("%s-service", pod.Name)
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: serviceName,
				Labels: map[string]string{
					"app": pod.Labels["app"],
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"app":  pod.Labels["app"],
					"node": pod.Spec.NodeName,
				},
				Ports: []corev1.ServicePort{
					{
						Port:       80,
						TargetPort: intstr.FromInt(8080),
					},
				},
				Type: corev1.ServiceTypeClusterIP,
			},
		}

		_ = clientset.CoreV1().Services(namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		for {
			_, err := clientset.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				break
			}
		}

		createdService, err := clientset.CoreV1().Services(namespace).Create(context.TODO(), service, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create service for pod %s: %v", pod.Name, err)
		}

		nodeClusterIPs = append(nodeClusterIPs, NodeClusterIP{
			NodeName:  pod.Spec.NodeName,
			ClusterIP: createdService.Spec.ClusterIP,
		})
	}

	return nodeClusterIPs, nil
}
