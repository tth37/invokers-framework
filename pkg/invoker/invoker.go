package invoker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"log"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// FunctionState represents the current state of a function
type FunctionState string

const (
	Cold    FunctionState = "cold"
	Ensured FunctionState = "ensured"
	Warm    FunctionState = "warm"
)

// Invoker handles function invocation and maintains function states
type Invoker struct {
	client        *kubernetes.Clientset
	namespace     string
	nodeName      string
	functionState map[string]FunctionState
	ipTable       map[string]string
	mu            sync.RWMutex
}

// NewInvoker initializes a new Invoker with the given Kubernetes configuration and namespace
func NewInvoker(config *rest.Config, nodeName string, namespace string) (*Invoker, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	return &Invoker{
		client:        clientset,
		nodeName:      nodeName,
		namespace:     namespace,
		functionState: make(map[string]FunctionState),
		ipTable:       make(map[string]string),
	}, nil
}

// ensure checks if the function image is available locally and pulls it if not
func (i *Invoker) ensure(fn string) error {
	// Create a valid pod name from the function name
	podName := createValidPodName("ensure", i.nodeName, fn)

	// Clean up any existing ensure pods (if any)
	_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
	// Wait until the pod is deleted
	for {
		_, err := i.client.CoreV1().Pods(i.namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Create a Pod to pull the image
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "ensure",
					Image:   fn,
					Command: []string{"sh", "-c", "echo Image pulled successfully && sleep 5"},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			// Execute the Pod on the same node as the Invoker
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "kubernetes.io/hostname",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{i.nodeName},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Create the Pod in the specified namespace
	createdPod, err := i.client.CoreV1().Pods(i.namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create ensure pod: %v", err)
	}

	// Ensure the pod is deleted after we're done
	defer func() {
		deletePolicy := metav1.DeletePropagationForeground
		if err := i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), createdPod.Name, metav1.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}); err != nil {
			fmt.Printf("Warning: failed to delete pod %s: %v\n", createdPod.Name, err)
		}
	}()

	// Wait for the Pod to be ready or fail
	err = i.waitForPod(createdPod.Name, 1*time.Minute)
	if err != nil {
		return err
	}

	log.Printf("Ensured function %s", fn)

	return nil
}

func (i *Invoker) waitForPod(podName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pod %s to be ready", podName)
		default:
			pod, err := i.client.CoreV1().Pods(i.namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get pod %s: %v", podName, err)
			}

			switch pod.Status.Phase {
			case corev1.PodRunning, corev1.PodSucceeded:
				return nil
			case corev1.PodFailed:
				return fmt.Errorf("pod %s failed", podName)
			case corev1.PodPending:
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						reason := containerStatus.State.Waiting.Reason
						if reason == "ErrImagePull" || reason == "ImagePullBackOff" {
							return fmt.Errorf("failed to pull image for pod %s: %s", podName, reason)
						}
					}
				}
			}

			time.Sleep(1 * time.Millisecond)
		}
	}
}

func (i *Invoker) waitForPodHealthy(clusterIP string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := &http.Client{
		Timeout: 50 * time.Millisecond,
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pod to be healthy")
		default:
			url := fmt.Sprintf("http://%s:8080/_/healthy", clusterIP)

			resp, err := client.Get(url)
			log.Printf("GET %s: %v", url, err)

			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return nil
			}
			if resp != nil {
				resp.Body.Close()
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}

// createValidPodName generates a valid pod name from a prefix and function name
func createValidPodName(prefix string, nodeName string, fn string) string {
	// Hash fn to get a unique 6-character name
	hash := uuid.NewSHA1(uuid.NameSpaceDNS, []byte(fn)).String()
	name := hash[:6]

	// Combine prefix and name
	fullName := fmt.Sprintf("%s-%s-%s", prefix, nodeName, name)

	// Limit the name to 63 characters (Kubernetes limit for pod names)
	if len(fullName) > 63 {
		fullName = fullName[:63]
	}

	return fullName
}

// prewarm creates a running pod for the function
func (i *Invoker) prewarm(fn string) (string, error) {

	// startAt := time.Now()

	// Create a valid name for the pod and service
	baseName := createValidPodName("prewarm", i.nodeName, fn)
	podName := baseName + "-pod"
	serviceName := baseName + "-svc"

	// Clean up any existing prewarm pods and services (if any)
	// _ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
	_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
	_ = i.client.CoreV1().Services(i.namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})

	// Wait until the pod is deleted
	for {
		_, err := i.client.CoreV1().Pods(i.namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// log.Printf("[Timer] Clean up: %v", time.Since(startAt))

	// Create a Pod to prewarm the function
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName + "-" + uuid.New().String()[0:5],
			Labels: map[string]string{
				"function": podName,
				"prewarm":  "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "prewarm",
					Image: fn,
					// Use default command to run the function
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			// Execute the Pod on the same node as the Invoker
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "kubernetes.io/hostname",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{i.nodeName},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Create the Pod in the specified namespace
	_, err := i.client.CoreV1().Pods(i.namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create prewarm pod: %v", err)
	}

	// Create a Service for the pod
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceName + "-" + uuid.New().String()[0:5],
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"function": podName,
				"prewarm":  "true",
			},
			Ports: []corev1.ServicePort{
				{
					Port:       8080,
					TargetPort: intstr.FromInt(8080),
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	// Create the Service in the specified namespace
	createdService, err := i.client.CoreV1().Services(i.namespace).Create(context.TODO(), service, metav1.CreateOptions{})
	if err != nil {
		// If service creation fails, delete the pod
		_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
		return "", fmt.Errorf("failed to create service: %v", err)
	}

	log.Printf("Created prewarm pod %s and service %s", podName, serviceName)

	// log.Printf("[Timer] Create pod and service: %v", time.Since(startAt))

	// Wait for the Pod to be ready
	// err = i.waitForPod(createdPod.Name, 1*time.Minute)
	// if err != nil {
	// 	// Clean up resources if pod fails to become ready
	// 	_ = i.client.CoreV1().Services(i.namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
	// 	_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
	// 	return "", err
	// }

	// log.Printf("[Timer] Wait for pod: %v", time.Since(startAt))

	// Wait for the Service to get a ClusterIP
	ip, err := i.waitForService(createdService.Name, 10*time.Second)
	if err != nil {
		// Clean up resources if service fails to get ClusterIP
		_ = i.client.CoreV1().Services(i.namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
		return "", err
	}

	// log.Printf("[Timer] Wait for service: %v", time.Since(startAt))

	// Wait for the Pod to be healthy
	err = i.waitForPodHealthy(ip, 10*time.Second)
	if err != nil {
		// Clean up resources if pod fails to become healthy
		_ = i.client.CoreV1().Services(i.namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
		return "", err
	}

	// log.Printf("[Timer] Wait for pod healthy: %v", time.Since(startAt))

	log.Printf("Prewarmed function %s, with ClusterIP %s", fn, ip)

	return ip, nil
}

// cleanPrewarmResources deletes all prewarmed pods and services
func (i *Invoker) cleanPrewarmResources(fn string) {
	baseName := createValidPodName("prewarm", i.nodeName, fn)
	podName := baseName + "-pod"

	labelSelector := fmt.Sprintf("function=%s,prewarm=true", podName)

	// List all pods with the specified label
	pods, err := i.client.CoreV1().Pods(i.namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})

	if err != nil {
		log.Printf("Failed to list pods: %v", err)
		return
	}

	// Delete all pods with the specified label
	for _, pod := range pods.Items {
		log.Printf("Deleting pod %s", pod.Name)
		// go func(podName string) {
		// 	_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
		// }(pod.Name)
		_ = i.client.CoreV1().Pods(i.namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		for {
			_, err := i.client.CoreV1().Pods(i.namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err != nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}

	// List all services with the specified label
	services, err := i.client.CoreV1().Services(i.namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})

	if err != nil {
		log.Printf("Failed to list services: %v", err)
		return
	}

	// Delete all services with the specified label
	for _, service := range services.Items {
		log.Printf("Deleting service %s", service.Name)
		// go func(serviceName string) {
		// 	_ = i.client.CoreV1().Services(i.namespace).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		// }(service.Name)
		_ = i.client.CoreV1().Services(i.namespace).Delete(context.TODO(), service.Name, metav1.DeleteOptions{})
		for {
			_, err := i.client.CoreV1().Services(i.namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
			if err != nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}

}

func (i *Invoker) waitForService(serviceName string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout waiting for service %s to get ClusterIP", serviceName)
		default:
			service, err := i.client.CoreV1().Services(i.namespace).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("failed to get service %s: %v", serviceName, err)
			}

			if service.Spec.ClusterIP != "" {
				return service.Spec.ClusterIP, nil
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}

// GetFunctionState returns the current state of a function
func (i *Invoker) GetFunctionState(fn string) FunctionState {
	i.mu.RLock()
	defer i.mu.RUnlock()

	if state, exists := i.functionState[fn]; exists {
		return state
	}
	return Cold
}

// SwitchFunctionState changes the state of a function
func (i *Invoker) SwitchFunctionState(fn string, state FunctionState) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	// If not already in the map, initialize the function state to Cold
	if _, exists := i.functionState[fn]; !exists {
		i.functionState[fn] = Cold
	}
	currentState := i.functionState[fn]

	// The target state cannot be Cold
	if state == Cold {
		return fmt.Errorf("cannot switch function state to Cold")
	}

	// If the function's current state matches the target state, do nothing
	if currentState == state {
		return nil
	}

	log.Printf("Switching function %s state from %s to %s", fn, currentState, state)

	switch state {
	case Ensured:
		switch currentState {
		case Cold:
			// Cold -> Ensured: Ensure the function image
			if err := i.ensure(fn); err != nil {
				return fmt.Errorf("failed to ensure function: %v", err)
			}
		case Warm:
			// Warm -> Ensured: Delete the prewarmed pod
			i.cleanPrewarmResources(fn)
			i.ipTable[fn] = ""
		}
	case Warm:
		// Cold -> Warm or Ensured -> Warm: Create a prewarmed pod
		ip, err := i.prewarm(fn)
		if err != nil {
			return fmt.Errorf("failed to prewarm function: %v", err)
		}
		i.ipTable[fn] = ip
	}

	// Update the function state
	i.functionState[fn] = state
	return nil
}

// InvocationResult represents the result of invoking a function
type InvocationResult struct {
	Output            []byte
	ColdStartDuration time.Duration
	RunningDuration   time.Duration
	StartAt           time.Time
	EndAt             time.Time
}

// Invoke calls the function with the given name and payload
func (i *Invoker) Invoke(fn string, payload []byte) (*InvocationResult, error) {
	result := InvocationResult{
		StartAt: time.Now(),
	}

	log.Printf("Invoking function %s", fn)

	// Ensure the function is in the Warm state
	if err := i.SwitchFunctionState(fn, Warm); err != nil {
		return nil, fmt.Errorf("failed to switch function state: %v", err)
	}

	result.ColdStartDuration = time.Since(result.StartAt)

	// Get the ClusterIP of the prewarmed pod
	i.mu.RLock()
	ip := i.ipTable[fn]
	i.mu.RUnlock()

	// Create a request to the function
	url := fmt.Sprintf("http://%s:8080", ip)
	req, err := http.NewRequest("POST", url, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	// Send the request
	client := &http.Client{
		Timeout: 10 * time.Second, // Set a short timeout for each request
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Read the response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	result.Output = respBody
	result.RunningDuration = time.Since(result.StartAt) - result.ColdStartDuration
	result.EndAt = time.Now()

	return &result, nil
}

// ProfilePrewarm performs 10 prewarm operations for the given function, and returns the average time taken
func (i *Invoker) ProfilePrewarm(fn string) (time.Duration, error) {
	// Ensure the function is in the Ensured state
	if err := i.SwitchFunctionState(fn, Ensured); err != nil {
		return 0, fmt.Errorf("failed to switch function state: %v", err)
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	// Perform 10 prewarm operations and measure the time taken
	total := 0 * time.Second
	for j := 0; j < 5; j++ {
		start := time.Now()
		if _, err := i.prewarm(fn); err != nil {
			return 0, fmt.Errorf("failed to prewarm function: %v", err)
		}
		total += time.Since(start)
		i.cleanPrewarmResources(fn)
	}

	return total / 5, nil
}
