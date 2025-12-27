# arXiv Cache API Documentation

## REST API Endpoints

All API endpoints are prefixed with `/api/v1/`. Responses are JSON with the following structure:

```json
{
  "success": true,
  "data": { ... },
  "error": "..."
}
```

### Papers

#### Get Paper
```
GET /api/v1/papers/{id}
```

Returns paper metadata, citation count, and references.

**Example:**
```bash
curl http://localhost:8080/api/v1/papers/2301.00001
```

**Response:**
```json
{
  "success": true,
  "data": {
    "paper": {
      "id": "2301.00001",
      "title": "...",
      "authors": "...",
      "abstract": "...",
      ...
    },
    "citedByCount": 42,
    "references": [...]
  }
}
```

#### Get Citations
```
GET /api/v1/papers/{id}/citations
```

Returns papers that this paper cites.

**Example:**
```bash
curl http://localhost:8080/api/v1/papers/2301.00001/citations
```

#### Get Cited By
```
GET /api/v1/papers/{id}/cited-by?limit=50
```

Returns papers that cite this paper.

**Query Parameters:**
- `limit` (optional): Maximum number of results (default: 50)

#### Get Citation Graph
```
GET /api/v1/papers/{id}/graph
```

Returns citation graph data for visualization (nodes and edges).

**Example:**
```bash
curl http://localhost:8080/api/v1/papers/2301.00001/graph
```

#### Fetch Paper
```
POST /api/v1/papers/{id}/fetch?pdf=true&source=true&embedding=true
```

Fetches and downloads a paper from arXiv.

**Query Parameters:**
- `pdf` (optional): Download PDF (default: false)
- `source` (optional): Download source (default: true)
- `embedding` (optional): Generate embedding after fetch (default: false)

**Example:**
```bash
curl -X POST "http://localhost:8080/api/v1/papers/2301.00001/fetch?source=true&embedding=true"
```

#### Export Paper
```
GET /api/v1/papers/{id}/export/{format}
```

Exports paper in various formats.

**Formats:**
- `bibtex` - BibTeX format (.bib)
- `ris` - RIS format (.ris)
- `json` - JSON format (.json)

**Example:**
```bash
curl http://localhost:8080/api/v1/papers/2301.00001/export/bibtex
```

### Search

#### Search Papers (Keyword)
```
GET /api/v1/search?q={query}&category={category}&limit={limit}
```

Searches papers by title and abstract using SQLite FTS5.

**Query Parameters:**
- `q` (required): Search query
- `category` (optional): Filter by category (e.g., "cs.AI")
- `limit` (optional): Maximum results (default: 20)

**Example:**
```bash
curl "http://localhost:8080/api/v1/search?q=transformer&category=cs.CL&limit=10"
```

**Response:**
```json
{
  "success": true,
  "data": {
    "papers": [...],
    "count": 10
  }
}
```

#### Search Papers (Streaming) ⚡ NEW
```
GET /api/v1/search/stream?q={query}&category={category}&limit={limit}
```

Streams search results via Server-Sent Events (SSE) for real-time UI updates.

**Headers:**
- `Accept: text/event-stream`

**Query Parameters:**
- `q` (required): Search query
- `category` (optional): Filter by category
- `limit` (optional): Maximum results (default: 20)

**SSE Events:**
```
data: {"type":"start","query":"transformer","total":150}

data: {"type":"result","paper":{"id":"1706.03762","title":"..."}}

data: {"type":"progress","current":10,"total":150}

data: {"type":"complete","count":20}
```

**Example (JavaScript):**
```javascript
const evtSource = new EventSource('/api/v1/search/stream?q=transformer');
evtSource.onmessage = (e) => {
  const data = JSON.parse(e.data);
  if (data.type === 'result') {
    // Append paper to results
    appendPaper(data.paper);
  }
};
```

#### Semantic Search ⚡ NEW
```
GET /api/v1/search/semantic?q={query}&limit={limit}
```

Searches papers using vector similarity. Finds papers by concept, not just keywords.

**Query Parameters:**
- `q` (required): Search query (natural language)
- `limit` (optional): Maximum results (default: 20)

**Note:** Requires embeddings to be generated first (see Embeddings section below).

**Example:**
```bash
curl "http://localhost:8080/api/v1/search/semantic?q=attention+mechanisms+in+neural+networks&limit=10"
```

**Response:**
```json
{
  "success": true,
  "data": {
    "results": [
      {
        "paperId": "1706.03762",
        "paper": {...},
        "similarity": 0.92
      }
    ],
    "count": 10,
    "query": "attention mechanisms in neural networks"
  }
}
```

#### Search PDF Content
```
GET /api/v1/search/pdf?q={query}&limit={limit}&fuzzy={boolean}
```

Searches within downloaded PDF content using fuzzy matching.

**Query Parameters:**
- `q` (required): Search query
- `limit` (optional): Maximum results (default: 50)
- `fuzzy` (optional): Enable fuzzy matching (default: false)

### Embeddings ⚡ NEW

Embeddings enable semantic search by converting paper titles and abstracts into 384-dimensional vectors.

#### Generate Paper Embedding
```
POST /api/v1/papers/{id}/embeddings
```

Generates an embedding for a single paper.

**Example:**
```bash
curl -X POST "http://localhost:8080/api/v1/papers/1706.03762/embeddings"
```

**Response:**
```json
{
  "success": true,
  "data": {
    "paperId": "1706.03762",
    "message": "embedding generated successfully"
  }
}
```

#### Generate All Embeddings
```
POST /api/v1/embeddings/generate?limit={limit}
```

Generates embeddings for all papers in the cache.

**Query Parameters:**
- `limit` (optional): Process only N papers

**Headers:**
- `Accept: text/event-stream` - Get real-time progress via SSE

**Standard Response:**
```json
{
  "success": true,
  "data": {
    "count": 1000,
    "message": "embeddings generated successfully"
  }
}
```

**SSE Events (with Accept: text/event-stream):**
```
data: {"type":"start","message":"Starting embedding generation..."}

data: {"type":"progress","current":100,"total":1000,"message":"Processed 100/1000 papers (10%)"}

data: {"type":"complete","count":1000,"message":"Embedding generation completed successfully"}
```

**CLI Alternative:**
```bash
# Generate embeddings for all papers
arxiv reindex --embeddings

# Generate with limit
arxiv reindex --embeddings --limit 1000

# Auto-generate when fetching
arxiv fetch --with-embedding 2301.00001

# Use Python script directly
python3 tools/generate_embeddings.py ~/.cache/arxiv --limit 1000
```

### Categories

#### List Categories
```
GET /api/v1/categories
```

Returns all categories with paper counts.

**Example:**
```bash
curl http://localhost:8080/api/v1/categories
```

**Response:**
```json
{
  "success": true,
  "data": [
    {"name": "cs.AI", "count": 1234},
    {"name": "cs.CL", "count": 567},
    ...
  ]
}
```

### Statistics

#### Get Cache Statistics
```
GET /api/v1/stats
```

Returns cache statistics.

**Example:**
```bash
curl http://localhost:8080/api/v1/stats
```

**Response:**
```json
{
  "success": true,
  "data": {
    "totalPapers": 10000,
    "pdfsDownloaded": 5000,
    "sourcesDownloaded": 8000,
    "queuedDownloads": 0
  }
}
```

## Rate Limiting

API requests are rate-limited to 100 requests per minute per IP address. When rate limit is exceeded, the API returns HTTP 429 (Too Many Requests).

## Caching

API responses are cached for 5 minutes. Use `If-None-Match` header with ETag for conditional requests:

```bash
curl -H "If-None-Match: \"abc123\"" http://localhost:8080/api/v1/papers/2301.00001
```

If the resource hasn't changed, you'll get HTTP 304 (Not Modified).

## Web Export Endpoints

Papers can also be exported via web interface:

- `/paper/{id}/export/bibtex` - BibTeX export
- `/paper/{id}/export/ris` - RIS export
- `/paper/{id}/export/json` - JSON export

## Examples

### Fetch and export a paper
```bash
# Fetch paper
curl -X POST "http://localhost:8080/api/v1/papers/2301.00001/fetch?source=true"

# Export as BibTeX
curl http://localhost:8080/api/v1/papers/2301.00001/export/bibtex > paper.bib
```

### Search and get details
```bash
# Search
curl "http://localhost:8080/api/v1/search?q=attention&limit=5"

# Get details for first result
curl http://localhost:8080/api/v1/papers/1706.03762
```

### Get citation graph
```bash
curl http://localhost:8080/api/v1/papers/2301.00001/graph | jq '.data.nodes | length'
```

