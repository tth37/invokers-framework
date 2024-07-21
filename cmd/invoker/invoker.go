package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/tth37/step-invokers/cmd/utils"
	"github.com/tth37/step-invokers/pkg/invoker"
)

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

var inv *invoker.Invoker

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // default port
	}

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "default" // default namespace
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatalf("NODE_NAME is required")
	}

	// Initialize Kubernetes config
	config, err := utils.GetKubeConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	// Initialize Invoker
	inv, err = invoker.NewInvoker(config, nodeName, namespace)
	if err != nil {
		log.Fatalf("Failed to create Invoker: %v", err)
	}

	http.HandleFunc("/invoke", handleInvoke)
	http.HandleFunc("/ensure", handleEnsure)
	http.HandleFunc("/prewarm", handlePrewarm)

	log.Printf("Server starting on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("Received request: %+v\n", req)

	invResult, err := inv.Invoke(req.Function, []byte(req.Input))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Invocation result: %+v\n", invResult)

	result := Result{
		Output:            string(invResult.Output),
		ColdStartDuration: invResult.ColdStartDuration,
		RunningDuration:   invResult.RunningDuration,
		StartAt:           invResult.StartAt,
		EndAt:             invResult.EndAt,
	}

	log.Printf("Result: %+v\n", result)

	// If there are no more steps, return the output
	if len(req.Steps) == 0 {
		w.Header().Set("Content-Type", "application/json")
		resp := Response{
			Output:  string(invResult.Output),
			Results: []Result{result},
		}
		json.NewEncoder(w).Encode(resp)
		return
	}

	newReq := Request{
		Input:    string(invResult.Output),
		Function: req.Steps[0].Function,
		Steps:    req.Steps[1:], // Remove the current step
	}

	// Send request to the next step
	resp, err := sendRequest(req.Steps[0].IP, newReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Append to the beginning of the results
	resp.Results = append([]Result{result}, resp.Results...)

	// Return the response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleEnsure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	var req string
	
	// Simply view the request body as a string
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req = string(body)

	log.Printf("Received ensure request: %s\n", req)

	if err := inv.SwitchFunctionState(req, invoker.Ensured); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func handlePrewarm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	var req string
	// if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
	// 	log.Printf("Failed to decode request: %v", err)
	// 	http.Error(w, err.Error(), http.StatusBadRequest)
	// 	return
	// }

	// Simply view the request body as a string
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req = string(body)

	log.Printf("Received prewarm request: %s\n", req)

	if err := inv.SwitchFunctionState(req, invoker.Warm); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
