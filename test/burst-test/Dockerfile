FROM python:3.9-slim

# Copy the application code
COPY . /app/
# COPY config.yaml /app/config.yaml
# Copy the requirements file
COPY requirements.txt .

# Install Python dependencies
RUN pip install --no-cache-dir -r requirements.txt

# Set working directory
WORKDIR /app

# Run the application
# CMD ["python", "stress_app.py", "/app/config.yaml", "stress-test-x2"]
CMD ["python", "stress_app.py"]