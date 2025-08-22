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
`transcribe` is a radio communication monitoring service that provides early notifications and short reports of trail rescue operations. It automatically transcribes radio communications from NORCOM Fire Department channels and uses AI analysis to identify trail rescue activities.

The service processes audio files from fire dispatch channels, intelligently detects when trail rescue operations begin, and provides timely notifications via Slack.

### Key Features

1. **Intelligent Radio Monitoring**  
   Automatically processes WAV audio files from NORCOM Fire Department channels, filtering for relevant talkgroups and identifying trail rescue operations through AI-powered transcript analysis.

2. **Dynamic Channel Activation**  
   When trail rescues are detected on Fire Dispatch channels, the system automatically enables monitoring of assigned tactical channels (TAC1-TAC10) for the duration of the operation.

3. **Early Notification System**  
   Provides timely Slack notifications with call details, transcripts, and live audio links from OpenMHz.com.

4. **Scalable Processing Architecture**  
   Configurable worker pool processes multiple audio files concurrently using Apache Pulsar for reliable message queuing and S3 for audio file storage.

5. **Flexible AI Backend Support**  
   Supports multiple ML backends (Ollama and OpenAI) for dispatch message analysis, allowing deployment flexibility based on infrastructure requirements and cost considerations.

6. **Comprehensive Integration**  
   Combines ASR transcription, configurable AI analysis, Redis caching for channel management, and Slack messaging in a unified monitoring pipeline with full observability.

<details>
<summary><strong>System Flow</strong></summary>

```mermaid
flowchart TD
    A[Radio Audio Files] --> B[S3 Bucket]
    B --> C[S3 Event Notification]
    C --> D[Apache Pulsar Queue]
    D --> E[Transcribe Workers]
    
    E --> F{File Filter<br/>WAV + Known Talkgroup?}
    F -->|Yes| G[Download & Transcribe Audio]
    F -->|No| Z[Skip Processing]
    
    G --> H[ASR Service]
    H --> I[Transcript Generated]
    
    I --> J{Fire Dispatch<br/>Channel?}
    J -->|Yes| K[ML Backend Analysis<br/>Ollama or OpenAI]
    J -->|No| R{TAC Channel<br/>Active?}
    
    K --> M{Trail Rescue<br/>Detected?}
    M -->|Yes| N[Extract TAC Channel Info]
    M -->|No| L[Log & Store]
    
    N --> O[Store Thread ID in Dragonfly]
    O --> P[Send Initial Slack Alert]
    P --> Q[Include Call Details & OpenMHz Link]
    
    R -->|Yes| S[Get Thread ID from Dragonfly]
    R -->|No| L
    S --> T[Post to Slack Thread]
    
    Q --> U[Set TTL Timer]
    U --> V[Monitor TAC Channel]
    V --> W[Auto-Send Channel Closed Message]
    
    T --> L
    W --> X[Expires from Dragonfly]
    Q --> L
    
    style A fill:#e1f5fe
    style P fill:#c8e6c9
    style K fill:#fff3e0
    style O fill:#f3e5f5
    style T fill:#e8f5e8
    style W fill:#fff8dc
```

</details>

### Architecture Benefits

This monitoring system provides several operational advantages:

1. **Early Awareness**  
   Detects trail rescue operations as soon as they're dispatched.

2. **Automated Channel Management**  
   Dynamically enables tactical channel monitoring based on dispatch assignments, reducing manual coordination overhead.

3. **Reliable Processing**  
   Message queuing and worker pools ensure continuous monitoring even during high radio traffic periods or component failures.

4. **Contextual Notifications**  
   Slack integration provides rich context including transcripts, call types, and direct links to live audio streams for immediate situational awareness.

5. **Observability & Maintenance**  
   Comprehensive logging, metrics, and tracing enable monitoring of system health and rapid troubleshooting of processing issues.

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

- **Apache Pulsar**: Message broker for S3 events and processing queues
- **S3 Ninja**: S3-compatible storage for radio audio files  
- **Mock ASR**: Simulated transcription service for testing
- **Ollama**: AI service running Llama 3.1 8B for dispatch analysis (default ML backend)
- **Dragonfly**: Redis-compatible cache for tactical channel management
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
# Core Services
PULSAR_URL=pulsar://localhost:6650
PULSAR_INPUT_TOPIC=public/transcribe/file-queue
PULSAR_SUBSCRIPTION=transcribe-consumer
S3_ENDPOINT=http://localhost:9444
S3_BUCKET=audio
DRAGONFLY_ADDRESS=localhost:6379

# ML Backend Selection
ML_BACKEND=ollama  # Options: "ollama" or "openai"

# Ollama Configuration (when ML_BACKEND=ollama)
OLLAMA_PROTOCOL=http
OLLAMA_HOST=localhost:11434
OLLAMA_MODEL=llama3.1:8b
OLLAMA_TIMEOUT=15s

# OpenAI Configuration (when ML_BACKEND=openai)
OPENAI_API_KEY=your-openai-api-key
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_MODEL=gpt-4
OPENAI_TIMEOUT=30s

# ASR Service
ASR_ENDPOINT=http://localhost:8080/asr

# Notifications
SLACK_TOKEN=xoxb-your-bot-token
SLACK_CHANNEL_ID=C1234567890

# Operational
WORKER_COUNT=5
LOG_LEVEL=info
METRICS_ENABLED=true
```

#### ML Backend Selection

The service supports two ML backends for analyzing dispatch messages:

**Ollama (Default)**
- Runs locally with Llama 3.1 8B model
- No external API costs
- Requires local GPU/CPU resources
- Ideal for on-premise deployments

**OpenAI**
- Uses OpenAI's GPT models via API
- Requires API key and incurs usage costs
- Faster processing with high-quality analysis
- Ideal for cloud deployments

To switch between backends, set `ML_BACKEND=openai` and provide your `OPENAI_API_KEY`. The service will automatically validate the configuration and initialize the appropriate backend at startup.

### Testing

To test the radio monitoring pipeline:

```bash
# Push a test audio file simulating a fire dispatch call
make push-message

# Monitor processing and AI analysis in the logs  
docker-compose logs -f main

# Check Grafana dashboards at http://localhost:3000
```

The test script uploads `demo.m4a` as a simulated fire dispatch audio file and triggers the complete pipeline: transcription, AI analysis for trail rescue detection, and Slack notification (if configured). Monitor the logs to see the transcript analysis and any tactical channel activations.

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