FROM golang:1.24-alpine AS builder
WORKDIR /build

# Use modernc.org/sqlite (pure Go, no CGO required)
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy && go build -o arxiv-server ./cmd/arxiv

FROM python:3.11-slim

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    poppler-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy Go binary
COPY --from=builder /build/arxiv-server .

# Copy Python tools and install CPU-only PyTorch + dependencies (no NVIDIA/CUDA)
COPY tools/ /app/tools/
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu && \
    pip install --no-cache-dir -r /app/tools/requirements.txt

EXPOSE 80
ENV ARXIV_CACHE=/data/arxiv
CMD ["./arxiv-server", "serve", "-port", "80"]
