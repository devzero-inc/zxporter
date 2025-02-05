import yaml
import time
import sys
import requests  # used to query Prometheus

# --- Helper functions to parse resource strings ---
def parse_cpu(cpu_str):
    if cpu_str.endswith('mi'):
        return int(cpu_str[:-2]) / 1000
    elif cpu_str.endswith('m'):
        return int(cpu_str[:-1]) / 1000
    else:
        return float(cpu_str)

def parse_memory(mem_str):
    if mem_str.endswith('mb'):
        return int(mem_str[:-2]) * 10**6
    elif mem_str.endswith('Mi'):
        return int(mem_str[:-2]) * 1024**2
    elif mem_str.endswith('Gi'):
        return int(mem_str[:-2]) * 1024**3
    else:
        return int(mem_str)

def load_config(config_path):
    with open(config_path, 'r') as f:
        config = yaml.safe_load(f)
    test_config = config['test']
    duration = int(test_config['duration'][:-1])
    base = test_config['base']
    base_cpu = parse_cpu(base['cpu'])
    base_memory = parse_memory(base['memory'])
    bursts = []
    if 'burst' in test_config and 'times' in test_config['burst']:
        for burst_time_str, burst_params in test_config['burst']['times'].items():
            burst = {
                'start': int(burst_time_str),
                'duration': int(burst_params['duration'][:-1]),
                'cpu': parse_cpu(burst_params['cpu']) if 'cpu' in burst_params else None,
                'memory': parse_memory(burst_params['memory']) if 'memory' in burst_params else None
            }
            bursts.append(burst)
    return {
        'duration': duration,
        'base_cpu': base_cpu,
        'base_memory': base_memory,
        'bursts': sorted(bursts, key=lambda x: x['start'])
    }

# --- Function to query Prometheus for CPU usage ---
def get_current_cpu_usage(prometheus_url):
    """
    Query Prometheus for the current CPU usage of the container.
    Returns the value as a float in seconds per second.
    """
    query = ('max(rate(container_cpu_usage_seconds_total'
             '{namespace="default", container="stress-test"}[1m])) by (container)')
    url = prometheus_url.rstrip("/") + "/api/v1/query"
    try:
        response = requests.get(url, params={'query': query}, timeout=5)
        response.raise_for_status()
        data = response.json()
        if data.get('status') == 'success' and data.get('data', {}).get('result'):
            # Assume the first result is our container.
            value = data['data']['result'][0]['value'][1]
            return float(value)
    except Exception as e:
        print(f"Error querying Prometheus: {e}")
    return 0.0

def run_stress_test(config, prometheus_url):
    duration = config['duration']
    base_cpu = config['base_cpu']
    base_memory = config['base_memory']
    bursts = config['bursts']

    # Allocate the initial memory
    current_memory = base_memory
    try:
        memory_holder = bytearray(current_memory)
    except MemoryError:
        print(f"Initial memory allocation failed for {current_memory} bytes.")
        return

    # --- PID controller parameters ---
    kp = 0.1
    ki = 0.01
    kd = 0.0

    error_integral = 0.0
    last_error = 0.0

    # Start with the base busy duration (in seconds).
    busy_duration = base_cpu  # e.g., 0.08 for 80mi

    # Additional adjustment factor from Prometheus results.
    gamma = 0.05  # small step increase factor

    # Run the test for the specified number of seconds.
    for current_time in range(duration):
        current_cpu = base_cpu
        current_memory = base_memory

        # Check if a burst is active for this second.
        active_bursts = [burst for burst in bursts
                         if current_time >= burst['start'] and
                         current_time < burst['start'] + burst['duration']]
        for burst in active_bursts:
            if burst['cpu'] is not None:
                current_cpu = burst['cpu']
            if burst['memory'] is not None:
                current_memory = burst['memory']

        # Adjust memory allocation if needed.
        if len(memory_holder) != current_memory:
            try:
                memory_holder = bytearray(current_memory)
            except MemoryError:
                print(f"Memory allocation failed for {current_memory} bytes.")

        cycle_start = time.time()
        # Ensure busy_duration is within 0.0 to 1.0 seconds.
        busy_duration = max(0.0, min(busy_duration, 1.0))

        # Busy loop for CPU load.
        busy_loop_start = time.time()
        while (time.time() - busy_loop_start) < busy_duration:
            pass
        measured_busy = time.time() - busy_loop_start

        # --- PID controller based on local busy time ---
        error = current_cpu - measured_busy
        error_integral += error
        error_derivative = error - last_error

        busy_duration = busy_duration + kp * error + ki * error_integral + kd * error_derivative

        # --- Query Prometheus and adjust if actual usage is lower ---
        prom_cpu_usage = get_current_cpu_usage(prometheus_url)
        error_prom = current_cpu - prom_cpu_usage
        if error_prom > 0:
            # If Prometheus reports lower usage than desired, gently increase busy_duration.
            busy_duration += gamma * error_prom

        busy_duration = max(0.0, min(busy_duration, 1.0))
        last_error = error

        # Sleep until the 1-second cycle completes.
        elapsed = time.time() - cycle_start
        remaining = 1.0 - elapsed
        if remaining > 0:
            time.sleep(remaining)

        # Print the cycle status.
        print(f"Time: {current_time + 1:3d}s | "
              f"Target CPU: {current_cpu*1000:.0f}mi | "
              f"Measured Busy: {measured_busy*1000:.0f}ms | "
              f"Prometheus CPU: {prom_cpu_usage*1000:.0f}mi | "
              f"Memory: {current_memory/10**6:.0f}MB | "
              f"PID Busy Duration: {busy_duration*1000:.0f}ms")

    # Free the allocated memory
    del memory_holder

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: python stress_test.py <config.yaml>")
        sys.exit(1)
    config = load_config(sys.argv[1])
    # Set the Prometheus URL as provided.
    prometheus_url = "http://prometheus-service.monitoring.svc.cluster.local:8080"
    run_stress_test(config, prometheus_url)
