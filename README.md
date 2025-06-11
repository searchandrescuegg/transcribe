<h2 align="center">
  <!-- <img src=".github/images/transcribe-logo.png" alt="transcribe logo" width="500"> -->
  transcribe
</h2>
<h2 align="center">
  A scalable Go service for automated audio transcription using ASR (Automatic Speech Recognition) with Apache Pulsar messaging and S3 storage
</h2>
<div align="center">

&nbsp;&nbsp;&nbsp;[Docker Compose][docker-compose-link]&nbsp;&nbsp;&nbsp;|&nbsp;&nbsp;&nbsp;[Contributing][contributing-link]&nbsp;&nbsp;&nbsp;|&nbsp;&nbsp;&nbsp;[GitHub][organization-link]

[![Made With Go][made-with-go-badge]][for-the-badge-link]

</div>

---
## Overview
`transcribe` is a distributed Go service designed for high-throughput audio transcription processing. It integrates Apache Pulsar for message queuing, S3-compatible storage for audio file management, and external ASR services for speech-to-text conversion. The service is built with observability in mind, featuring comprehensive metrics, tracing, and logging capabilities.

The service follows a worker-pool architecture where multiple concurrent workers consume file processing messages from Pulsar, download audio files from S3, send them to an ASR endpoint for transcription, and publish the results back to Pulsar for downstream processing.

### Key Features

1. **Scalable Worker Architecture**  
   Configurable worker pool size allows horizontal scaling to handle varying transcription workloads. Each worker operates independently, ensuring fault isolation and optimal resource utilization.

2. **Reliable Message Processing**  
   Apache Pulsar integration provides guaranteed message delivery, partitioned topics for load distribution, and subscription-based consumption patterns that ensure no audio files are lost during processing.

3. **Cloud-Native Storage**  
   S3-compatible storage support (including AWS S3, MinIO, and other S3-compatible services) enables secure, scalable audio file storage with proper access controls and regional distribution.

4. **Comprehensive Observability**  
   Built-in OpenTelemetry integration provides distributed tracing, Prometheus metrics collection, and structured JSON logging for complete visibility into service performance and health.

5. **Flexible ASR Integration**  
   RESTful API design allows integration with any ASR service that accepts multipart file uploads, making it compatible with popular services like AWS Transcribe, Google Speech-to-Text, or custom ASR endpoints.

### Architecture Benefits

Using this distributed architecture provides several advantages:

1. **High Availability**  
   Multiple workers and message queuing ensure the service continues processing even if individual components fail. Pulsar's built-in replication and persistence guarantee message durability.

2. **Elastic Scaling**  
   Worker count can be adjusted based on processing demand. The service scales horizontally by adding more instances, and vertically by increasing worker count per instance.

3. **Fault Tolerance**  
   Failed transcription attempts are logged but don't block other processing. The message queue ensures failed messages can be retried or handled by dead letter queues.

4. **Monitoring & Debugging**  
   Structured logging, distributed tracing, and comprehensive metrics enable rapid identification of bottlenecks, errors, and performance issues in production environments.

5. **Storage Flexibility**  
   S3-compatible storage abstraction allows deployment across different cloud providers or on-premises environments without code changes.

By utilizing this service, organizations can build robust audio processing pipelines that scale from hundreds to millions of audio files while maintaining reliability and observability.

## Development

### Prerequisites

**Docker & Docker Compose**  
  For local development and testing:
1. Install Docker Desktop from the [official Docker website](https://www.docker.com/products/docker-desktop/).
2. Ensure Docker Compose is included (it comes with Docker Desktop by default).
3. Verify installation by running `docker --version` and `docker-compose --version`.

**Node.js** (for commit hooks)  
  To set up commit linting:
1. Install Node.js LTS from the [official Node.js website](https://nodejs.org/).
2. Use `nvm use --lts` if you have nvm installed.
3. Run `make setup` to install commitlint dependencies.

### Local Development

This service includes a complete local development environment using Docker Compose with the following components:

- **Apache Pulsar**: Message broker for file processing queues
- **S3 Ninja**: S3-compatible storage for audio files  
- **Mock ASR**: Simulated ASR service for testing
- **Grafana LGTM**: Observability stack (Logs, Grafana, Tempo, Mimir)

To start the development environment:

```bash
# Start all services
docker-compose up -d

# View logs
docker-compose logs -f main

# Stop services
docker-compose down
```

The service exposes the following ports:
- `8081`: Prometheus metrics endpoint
- `3000`: Grafana dashboard
- `6650`: Pulsar broker
- `9444`: S3 Ninja web interface
- `1080`: Mock ASR service

### Configuration

The service is configured via environment variables. Key settings include:

```bash
# Logging
LOG_LEVEL=info

# Pulsar Configuration
PULSAR_URL=pulsar://localhost:6650
PULSAR_INPUT_TOPIC=public/transcribe/file-queue
PULSAR_OUTPUT_TOPIC=public/transcribe/transcription-results
PULSAR_SUBSCRIPTION=transcribe-consumer

# S3 Configuration
S3_REGION=us-east-1
S3_BUCKET=audio
S3_ENDPOINT=http://localhost:9444

# ASR Configuration
TARGET_ENDPOINT=http://localhost:1080/transcribe

# Worker Configuration
WORKER_COUNT=5
```

### Testing

To test the transcription pipeline:

```bash
# Push a test audio file to the processing queue
make push-message

# Monitor processing in the logs
docker-compose logs -f main

# Check Grafana dashboards at http://localhost:3000
```

The test script pushes `demo.m4a` to the S3 bucket and publishes a processing message to Pulsar. You can monitor the complete pipeline from file upload through transcription to result publication.

<!--

Reference Variables

-->

<!-- Badges -->
[made-with-go-badge]: .github/images/made-with-go.svg

<!-- Links -->
[docker-compose-link]: #local-development
[contributing-link]: #development
[organization-link]: https://github.com/searchandrescuegg
[for-the-badge-link]: https://forthebadge.com