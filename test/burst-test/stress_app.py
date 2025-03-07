# import yaml
# import subprocess
# import time
# import threading

# # Load the YAML configuration file
# def load_config(config_file):
#     with open(config_file, 'r') as file:
#         config = yaml.safe_load(file)
#     return config

# # Function to generate CPU load using CPULoadGenerator.py
# def generate_cpu_load(cpu_load, cores, duration):
#     command = ['./CPULoadGenerator/CPULoadGenerator.py', '-l', str(cpu_load), '-d', str(duration)]
#     for core in cores:
#         command.extend(['-c', str(core)])
#     subprocess.run(command)

# # Function to simulate memory consumption
# def consume_memory(memory_mb, duration):
#     # Allocate memory by creating a list of bytes
#     memory_consumption = ' ' * (memory_mb * 1024 * 1024)
#     time.sleep(duration)
#     del memory_consumption  # Free the memory

# # Function to handle burst scheduling
# def schedule_bursts(config):
#     base_cpu = config['test']['base']['cpu']
#     base_memory = config['test']['base']['memory']
#     total_duration = config['test']['duration']
#     burst_times = config['test']['burst']['times']

#     # Start base resource consumption
#     print(f"Starting base consumption: CPU={base_cpu}mi, Memory={base_memory}mb")
#     cpu_thread = threading.Thread(target=generate_cpu_load, args=(base_cpu / 1000, [0], total_duration))
#     memory_thread = threading.Thread(target=consume_memory, args=(base_memory, total_duration))
#     cpu_thread.start()
#     memory_thread.start()
    
#     prv_burst_time = 0
#     prv_duration = 0

#     # Schedule bursts
#     for burst_time, burst_config in burst_times.items():
#         burst_cpu = burst_config.get('cpu', base_cpu)
#         burst_memory = burst_config.get('memory', base_memory)
#         burst_duration = burst_config['duration']

#         # Calculate the start time for the burst
#         time.sleep(burst_time - (prv_burst_time + prv_duration))
#         print(f"Starting burst at {burst_time}s: CPU={burst_cpu}mi, Memory={burst_memory}mb for {burst_duration}s")
#         cpu_thread = threading.Thread(target=generate_cpu_load, args=((burst_cpu - base_cpu) / 1000, [0], burst_duration))
#         memory_thread = threading.Thread(target=consume_memory, args=((burst_memory - base_memory), burst_duration))
#         cpu_thread.start()
#         memory_thread.start()

#         # Wait for the burst duration before scheduling the next burst
#         time.sleep(burst_duration)
#         prv_duration = burst_duration
#         prv_burst_time = burst_time
        

#     # Wait for the base consumption to finish
#     cpu_thread.join()
#     memory_thread.join()

# # Main function
# def main():
#     config_file = '/app/config.yaml'  # Path to your YAML config file
#     config = load_config(config_file)
#     schedule_bursts(config)

# if __name__ == "__main__":
#     main()



import yaml
import subprocess
import time
import threading
import os

# Load the YAML configuration file
def load_config(config_file):
    with open(config_file, 'r') as file:
        config = yaml.safe_load(file)
    return config

# Function to detect available CPU cores
def get_available_cores():
    # Get the number of available CPU cores
    return os.cpu_count()

# Function to generate CPU load using CPULoadGenerator.py
def generate_cpu_load(cpu_load, cores, duration):
    command = ['./CPULoadGenerator/CPULoadGenerator.py', '-l', str(cpu_load), '-d', str(duration)]
    for core in cores:
        command.extend(['-c', str(core)])
    subprocess.run(command)

# Function to simulate memory consumption
def consume_memory(memory_mb, duration):
    # Allocate memory by creating a list of bytes
    memory_consumption = ' ' * (memory_mb * 1024 * 1024)
    time.sleep(duration)
    del memory_consumption  # Free the memory

# Function to handle burst scheduling
def schedule_bursts(config):
    base_cpu = config['test']['base']['cpu']
    base_memory = config['test']['base']['memory']
    total_duration = config['test']['duration']
    burst_times = config['test']['burst']['times']

    # Get available cores and select the first 4
    available_cores = get_available_cores()
    selected_cores = list(range(min(4, available_cores)))  # Use first 4 cores
    print(f"Selected cores: {selected_cores}")

    # Divide the CPU load equally among the selected cores
    cpu_load_per_core = (base_cpu / 1000) / len(selected_cores)
    print(f"Base CPU load per core: {cpu_load_per_core}")

    # Start base resource consumption
    print(f"Starting base consumption: CPU={base_cpu}mi, Memory={base_memory}mb")
    cpu_threads = []
    for core in selected_cores:
        thread = threading.Thread(target=generate_cpu_load, args=(cpu_load_per_core, [core], total_duration))
        cpu_threads.append(thread)
        thread.start()

    memory_thread = threading.Thread(target=consume_memory, args=(base_memory, total_duration))
    memory_thread.start()
    
    prv_burst_time = 0
    prv_duration = 0

    # Schedule bursts
    for burst_time, burst_config in burst_times.items():
        burst_cpu = burst_config.get('cpu', base_cpu) - base_cpu
        burst_memory = burst_config.get('memory', base_memory) - base_memory
        burst_duration = burst_config['duration']

        # Divide the burst CPU load equally among the selected cores
        burst_cpu_load_per_core = (burst_cpu / 1000) / len(selected_cores)
        print(f"Starting burst at {burst_time}s: CPU={burst_cpu}mi, Memory={burst_memory}mb for {burst_duration}s")

        # Wait until the burst time
        time.sleep(burst_time - (prv_burst_time + prv_duration))

        # Start burst threads
        burst_cpu_threads = []
        for core in selected_cores:
            thread = threading.Thread(target=generate_cpu_load, args=(burst_cpu_load_per_core, [core], burst_duration))
            burst_cpu_threads.append(thread)
            thread.start()

        burst_memory_thread = threading.Thread(target=consume_memory, args=(burst_memory, burst_duration))
        burst_memory_thread.start()

        # Wait for the burst duration
        time.sleep(burst_duration)

        # Wait for burst threads to finish
        for thread in burst_cpu_threads:
            thread.join()
        burst_memory_thread.join()
        prv_duration = burst_duration
        prv_burst_time = burst_time

    # Wait for the base consumption to finish
    for thread in cpu_threads:
        thread.join()
    memory_thread.join()

# Main function
def main():
    config_file = '/app/config.yaml'  # Path to your YAML config file
    config = load_config(config_file)
    schedule_bursts(config)

if __name__ == "__main__":
    main()