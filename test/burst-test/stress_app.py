import yaml
import time
import sys

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

def run_stress_test(config):
    duration = config['duration']
    base_cpu = config['base_cpu']
    base_memory = config['base_memory']
    bursts = config['bursts']

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

    busy_duration = base_cpu  # in seconds (e.g., 0.08 for 80mi)

    # Run the test for the specified number of seconds.
    for current_time in range(duration):
        current_cpu = base_cpu
        current_memory = base_memory

        active_bursts = [burst for burst in bursts
                         if current_time >= burst['start'] and
                         current_time < burst['start'] + burst['duration']]

        for burst in active_bursts:
            if burst['cpu'] is not None:
                current_cpu = burst['cpu']
            if burst['memory'] is not None:
                current_memory = burst['memory']

        if len(memory_holder) != current_memory:
            try:
                memory_holder = bytearray(current_memory)
            except MemoryError:
                print(f"Memory allocation failed for {current_memory} bytes.")

        cycle_start = time.time()
        busy_duration = max(0.0, min(busy_duration, 1.0))

        busy_loop_start = time.time()
        while (time.time() - busy_loop_start) < busy_duration:
            pass
        measured_busy = time.time() - busy_loop_start

        error = current_cpu - measured_busy
        error_integral += error
        error_derivative = error - last_error

        busy_duration = busy_duration + kp * error + ki * error_integral + kd * error_derivative
        busy_duration = max(0.0, min(busy_duration, 1.0))

        last_error = error

        elapsed = time.time() - cycle_start
        remaining = 1.0 - elapsed
        if remaining > 0:
            time.sleep(remaining)

        print(f"Time: {current_time + 1:3d}s | "
              f"Target CPU: {current_cpu*1000:.0f}mi | "
              f"Measured Busy: {measured_busy*1000:.0f}ms | "
              f"Memory: {current_memory/10**6:.0f}MB | "
              f"PID Busy Duration: {busy_duration*1000:.0f}ms")

    del memory_holder

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: python stress_test.py <config.yaml>")
        sys.exit(1)
    config = load_config(sys.argv[1])
    run_stress_test(config)
