
# Resource Adjustment Operator (Balance)

The  **Resource Adjustment Operator**  (codename:  **Balance**) is a Kubernetes operator designed to provide resource request recommendations for running containers in a Kubernetes cluster. It leverages a custom  **Anti-Windup PID Controller**  with a sliding window mechanism to dynamically adjust resource recommendations based on historical usage patterns.

----------

## Key Features

1.  **Resource Recommendation**:
    
    -   Provides CPU and memory request recommendations for containers based on historical usage data.
        
    -   Uses a sliding window mechanism to ensure recommendations are based on recent usage patterns, preventing indefinite windup from stale data.
        
2.  **Anti-Windup PID Controller**:
    
    -   Implements a custom PID controller with anti-windup and sliding window mechanisms.
        
    -   Ensures that recommendations are responsive to recent changes in resource usage while avoiding over-correction from outdated data.
        
3.  **Dynamic Frequency Adjustment**:
    
    -   Adjusts the frequency of recommendations based on the error between the current resource usage and the target utilization.
        
    -   Uses a normalized control signal to map the frequency between  `30s`  (min) and  `20m`  (max).
        
4.  **OOM Protection**:
    
    -   Detects OOM (Out-Of-Memory) events and adjusts memory recommendations to prevent future OOM kills.
        
5.  **Node Selection**:
    
    -   Evaluates whether the current node can accommodate the recommended resources.
        
    -   Suggests migration to a suitable node if the current node cannot handle the recommended resources.
        

----------

## How It Works

### 1.  **Resource Monitoring**

-   The operator periodically fetches CPU and memory usage metrics for each container using Prometheus.
    
-   It calculates the current utilization and compares it against the target utilization (e.g., 85% for CPU and memory).
    

### 2.  **Error Calculation**

-   The error is calculated as the difference between the target utilization and the current utilization.
    
-   The error is fed into the  **Anti-Windup PID Controller**  to compute the control signal.
    

### 3.  **Anti-Windup PID Controller**

-   The PID controller uses a sliding window of the last 15 errors to calculate the integral term.
    
-   The integral term is decayed over time to reduce the influence of older errors.
    
-   The controller ensures that the control signal does not grow indefinitely (anti-windup) by clamping the integral term during saturation.
    

### 4.  **Frequency Adjustment**

-   The control signal is normalized to a range of  `[-1.0, 1.0]`  and mapped to a frequency between  `30s`  and  `20m`.
    
-   A higher control signal results in more frequent recommendations, while a lower signal reduces the frequency.
    

### 5.  **Recommendation Calculation**

-   The operator calculates the recommended CPU and memory requests based on the 95th percentile of historical usage data.
    
-   Adjustments are made to ensure recommendations are within safe bounds (e.g., minimum CPU and memory requirements).
    

### 6.  **Node Evaluation**

-   The operator evaluates whether the current node can accommodate the recommended resources.
    
-   If the current node cannot handle the recommendations, it suggests migration to a suitable node.