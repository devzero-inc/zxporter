FROM python:3.9-slim

# Install stress-ng and dependencies
RUN apt-get update && apt-get install -y stress-ng && rm -rf /var/lib/apt/lists/*

# Copy the application code
COPY stress_app.py /app/stress_app.py
# COPY config.yaml /app/config.yaml
# Copy the requirements file
COPY requirements.txt .

# Install Python dependencies
RUN pip install --no-cache-dir -r requirements.txt

# Set working directory
WORKDIR /app

# Run the application
CMD ["python", "stress_app.py", "/app/config.yaml"]